package tlsca

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T, dir string, extra ...string) *Manager {
	t.Helper()
	m, err := Open(dir, extra, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("open tlsca in %s: %v", dir, err)
	}
	return m
}

// The whole point of the phase: a server that generates its own identity, and a
// client that validates it against the one installed file.
func TestAPinnedCAValidatesTheServer(t *testing.T) {
	m := open(t, t.TempDir())

	addr := serve(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(m.CACertPEM()) {
		t.Fatal("the CA it wrote is not PEM a client will load")
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}

	// Over 127.0.0.1, which is in the SAN set for exactly this reason: the
	// gallery and the CLI reach photod that way.
	resp, err := client.Get("https://" + addr)
	if err != nil {
		t.Fatalf("get over pinned TLS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// The negative half. Without the CA nothing validates, which is what makes
// installing it meaningful.
func TestAnUntrustedClientIsRefused(t *testing.T) {
	m := open(t, t.TempDir())

	addr := serve(t, m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}}
	if _, err := client.Get("https://" + addr); err == nil {
		t.Fatal("a client with only the system roots accepted a self-signed certificate")
	}
}

func TestReopeningReusesTheSameCA(t *testing.T) {
	dir := t.TempDir()
	first, _ := open(t, dir).Fingerprints()
	second, _ := open(t, dir).Fingerprints()

	if first != second {
		t.Fatalf("the CA changed across restarts (%s then %s); every paired device would break", first, second)
	}
}

// A corrupt CA has to stop the daemon. Quietly minting a replacement would leave
// every paired phone rejecting the server with nothing to explain why, and the
// choice between restoring the file and re-pairing is a human's.
func TestACorruptCARefusesToStart(t *testing.T) {
	dir := t.TempDir()
	open(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not a certificate"), 0o644); err != nil {
		t.Fatalf("corrupt the CA: %v", err)
	}
	if _, err := Open(dir, nil, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a corrupt CA was accepted")
	}
}

func TestHalfAnIdentityRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	m := open(t, dir)

	if err := os.Remove(m.caKeyPath()); err != nil {
		t.Fatalf("remove the CA key: %v", err)
	}
	if _, err := Open(dir, nil, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a certificate with no key was accepted")
	}
}

func TestTheCAKeyIsNotReadableByAnyoneElse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	// Created loosely on purpose: the deploy instructions make this directory
	// ahead of time, and opening it has to tighten it rather than trust it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pre-create the tls dir: %v", err)
	}
	m := open(t, dir)

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the tls dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("tls dir mode = %04o, want 0700", perm)
	}

	for _, path := range []string{m.caKeyPath(), m.leafKeyPath()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", filepath.Base(path), perm)
		}
	}
}

// The reason the leaf is disposable. A new address has to end up in the
// certificate, or the phone dials something the server cannot prove it is.
func TestAnExtraAddressIsCertified(t *testing.T) {
	dir := t.TempDir()
	open(t, dir)

	m := open(t, dir, "10.9.9.9", "photos.example.com")
	leaf := m.leafFor(t)

	if !hasIP(leaf.IPAddresses, "10.9.9.9") {
		t.Errorf("IP SANs = %v, want 10.9.9.9", leaf.IPAddresses)
	}
	var found bool
	for _, name := range leaf.DNSNames {
		if name == "photos.example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("DNS SANs = %v, want photos.example.com", leaf.DNSNames)
	}
}

// Refresh is a no-op when nothing moved, so the five-minute watch costs one read
// of the interface list rather than a certificate.
func TestRefreshIsQuietWhenNothingChanged(t *testing.T) {
	m := open(t, t.TempDir())
	before := m.leafFor(t).SerialNumber.String()

	reissued, err := m.Refresh()
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if reissued {
		t.Error("refresh reissued a certificate that was still correct")
	}
	if after := m.leafFor(t).SerialNumber.String(); after != before {
		t.Errorf("serial changed from %s to %s without a reason to reissue", before, after)
	}
}

// A leaf near expiry is replaced before it stops working, rather than after.
func TestAnExpiringLeafIsReissued(t *testing.T) {
	m := open(t, t.TempDir())
	before := m.leafFor(t).SerialNumber.String()

	m.mu.Lock()
	m.leafEnd = time.Now().Add(renewBefore / 2)
	m.mu.Unlock()

	reissued, err := m.Refresh()
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !reissued {
		t.Fatal("a leaf inside the renewal window was left alone")
	}
	if after := m.leafFor(t).SerialNumber.String(); after == before {
		t.Error("the certificate was reported reissued but the serial did not change")
	}
}

// iOS matches on SANs and needs serverAuth. A certificate missing either is
// rejected by the phone with an error that says nothing useful.
func TestTheLeafIsShapedTheWayIOSRequires(t *testing.T) {
	m := open(t, t.TempDir())
	leaf := m.leafFor(t)

	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("ExtKeyUsage = %v, want exactly serverAuth", leaf.ExtKeyUsage)
	}
	if leaf.IsCA {
		t.Error("the leaf is marked as a CA")
	}
	if !hasIP(leaf.IPAddresses, "127.0.0.1") {
		t.Errorf("IP SANs = %v, want 127.0.0.1 among them", leaf.IPAddresses)
	}
	if leaf.NotBefore.After(time.Now()) {
		t.Error("the leaf is not valid yet; clock skew would make it unusable")
	}
	// Verifying against the CA is what the phone does, and it is what catches a
	// missing serverAuth or a chain that does not actually chain.
	pool := x509.NewCertPool()
	pool.AddCert(m.caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSName:   "localhost",
	}); err != nil {
		t.Errorf("the leaf does not verify against its own CA: %v", err)
	}
}

func (m *Manager) leafFor(t *testing.T) *x509.Certificate {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.leaf == nil || m.leaf.Leaf == nil {
		t.Fatal("no leaf certificate")
	}
	return m.leaf.Leaf
}

func hasIP(ips []net.IP, want string) bool {
	target := net.ParseIP(want)
	for _, ip := range ips {
		if ip.Equal(target) {
			return true
		}
	}
	return false
}

// serve starts an HTTPS listener on the manager's certificate and returns its
// address.
//
// Deliberately not httptest.StartTLS. That helper appends its own certificate to
// the config, and Go only consults GetCertificate when Certificates is empty or
// the handshake carried an SNI name — so a client dialling by IP, which sends no
// SNI, would be served httptest's certificate instead of this one and the test
// would fail for a reason having nothing to do with the code. photod sets
// TLSConfig with no Certificates at all, which is what makes GetCertificate
// authoritative there.
func serve(t *testing.T, m *Manager, handler http.Handler) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", m.TLSConfig())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}
