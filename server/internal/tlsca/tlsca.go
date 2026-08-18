// Package tlsca gives photod a TLS identity it can create and maintain for
// itself, with no certificate authority and no domain name involved.
//
// Two certificates, with very different jobs. The CA is the trust anchor: it is
// generated once, lasts ten years, and is the file installed on the phone. The
// leaf is disposable — it is reissued whenever the machine's set of addresses
// changes or its expiry comes into view, and swapped in without a restart. That
// split is the point. A DHCP lease that hands the archive machine a new address,
// or a Tailscale interface that only comes up after photod has already started,
// would otherwise mean a certificate whose SANs no longer cover the address the
// phone dials — a backup that stops working for a reason nothing on either
// screen would explain. Reissuing the leaf fixes it in place, and because the CA
// never moves, the phone never has to be touched again.
package tlsca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// caLifetime is long because reinstalling the CA means walking to the phone.
	// Certificates issued by a user-installed root are exempt from the 398-day
	// ceiling Apple applies to publicly-trusted ones.
	caLifetime = 10 * 365 * 24 * time.Hour
	// leafLifetime is short because renewal is automatic and free.
	leafLifetime = 395 * 24 * time.Hour
	// renewBefore is how much runway a leaf gets. Comfortably longer than any
	// plausible gap between restarts of an always-on daemon.
	renewBefore = 30 * 24 * time.Hour

	// backdate absorbs clock skew between this machine and the phone. A
	// certificate that is not valid yet fails exactly as hard as an expired one
	// and is far more confusing.
	backdate = time.Hour
)

// Manager holds the current TLS identity and keeps it current.
type Manager struct {
	dir   string
	extra []string
	log   *slog.Logger

	mu      sync.RWMutex
	caCert  *x509.Certificate
	caDER   []byte
	caKey   *ecdsa.PrivateKey
	leaf    *tls.Certificate
	leafEnd time.Time
	sans    []string
}

// Open loads the identity in dir, creating whatever is missing. extraSANs are
// additional names or addresses to certify — an address the server is reached on
// that it cannot see from the inside, such as one behind a NAT.
func Open(dir string, extraSANs []string, log *slog.Logger) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	// 0700: ca.key lives here, and it is the one file in this project whose
	// disclosure lets somebody impersonate the archive to a paired phone. The
	// chmod is separate because MkdirAll leaves an existing directory's mode
	// alone, and the deploy instructions create this one ahead of time.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create tls dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure tls dir %s: %w", dir, err)
	}

	m := &Manager{dir: dir, extra: normalizeExtras(extraSANs), log: log}
	if err := m.loadOrCreateCA(); err != nil {
		return nil, err
	}
	if _, err := m.Refresh(); err != nil {
		return nil, err
	}
	return m, nil
}

// TLSConfig serves the current leaf. GetCertificate is read per handshake, so a
// certificate reissued while the server is running applies to the next
// connection without a restart.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			m.mu.RLock()
			defer m.mu.RUnlock()
			if m.leaf == nil {
				return nil, errors.New("tlsca: no certificate available")
			}
			return m.leaf, nil
		},
	}
}

// CACertPath is the file to install on the phone.
func (m *Manager) CACertPath() string { return filepath.Join(m.dir, "ca.crt") }

func (m *Manager) caKeyPath() string    { return filepath.Join(m.dir, "ca.key") }
func (m *Manager) leafCertPath() string { return filepath.Join(m.dir, "server.crt") }
func (m *Manager) leafKeyPath() string  { return filepath.Join(m.dir, "server.key") }

// Fingerprints are what iOS shows when a profile is installed, so printing both
// lets somebody check that what the phone is about to trust is what this machine
// generated. SHA-1 is here because Apple's dialog still shows it, not because it
// is worth anything on its own.
func (m *Manager) Fingerprints() (sha256Hex, sha1Hex string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s256 := sha256.Sum256(m.caDER)
	s1 := sha1.Sum(m.caDER)
	return colonHex(s256[:]), colonHex(s1[:])
}

// CACertPEM is the CA in the form a browser, a Go client, or an iOS profile
// wants it.
func (m *Manager) CACertPEM() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.caDER})
}

// SANs is what the current leaf certifies, for the startup log. A phone that
// cannot connect is usually dialling an address that is not in this list.
func (m *Manager) SANs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Clone(m.sans)
}

// NotAfter is when the current leaf stops being valid.
func (m *Manager) NotAfter() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.leafEnd
}

// Refresh reissues the leaf if it needs it, and reports whether it did.
//
// Three reasons to reissue: there is no usable leaf, the addresses it certifies
// are no longer the addresses this machine has, or it expires soon. Anything
// else is a no-op costing one directory read of the network interfaces.
func (m *Manager) Refresh() (bool, error) {
	wanted, err := wantedSANs(m.extra)
	if err != nil {
		return false, err
	}

	m.mu.RLock()
	current, leaf, end := m.sans, m.leaf, m.leafEnd
	m.mu.RUnlock()

	if leaf == nil {
		if loaded, ok := m.loadLeaf(wanted); ok {
			m.install(loaded)
			return false, nil
		}
	} else if slices.Equal(current, wanted) && time.Until(end) > renewBefore {
		return false, nil
	} else if slices.Equal(current, wanted) {
		m.log.Info("reissuing TLS certificate", "reason", "expiring", "not_after", end)
	} else {
		m.log.Info("reissuing TLS certificate", "reason", "addresses changed",
			"was", strings.Join(current, ","), "now", strings.Join(wanted, ","))
	}

	issued, err := m.issueLeaf(wanted)
	if err != nil {
		return false, err
	}
	m.install(issued)
	return true, nil
}

// Watch reissues the leaf when the machine's addresses change under it.
//
// The case this exists for is Tailscale: tailscaled may well bring its interface
// up after photod has started, and without this the Tailscale address stays out
// of the certificate until something restarts the daemon — so the archive is
// reachable at home and quietly unreachable away from it, which is the half of
// the setup least likely to be tested.
func (m *Manager) Watch(done <-chan struct{}, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if _, err := m.Refresh(); err != nil {
				m.log.Warn("could not refresh the TLS certificate", "error", err)
			}
		}
	}
}

type issued struct {
	cert *tls.Certificate
	end  time.Time
	sans []string
}

func (m *Manager) install(i issued) {
	m.mu.Lock()
	m.leaf, m.leafEnd, m.sans = i.cert, i.end, i.sans
	m.mu.Unlock()
}

func (m *Manager) loadOrCreateCA() error {
	certPEM, certErr := os.ReadFile(m.CACertPath())
	keyPEM, keyErr := os.ReadFile(m.caKeyPath())

	if certErr == nil && keyErr == nil {
		cert, key, err := parsePair(certPEM, keyPEM)
		if err != nil {
			// Refusing is the only safe answer. Silently minting a second CA
			// would leave every already-paired phone rejecting this server with
			// no hint as to why, and the recovery is a decision for a human:
			// restore the file, or delete it and re-pair every device.
			return fmt.Errorf("the CA in %s is unreadable (%w); restore it, or remove ca.crt and ca.key and re-pair every device", m.dir, err)
		}
		m.caCert, m.caDER, m.caKey = cert, cert.Raw, key
		return nil
	}
	if certErr == nil || keyErr == nil {
		return fmt.Errorf("half of the CA is present in %s: ca.crt %v, ca.key %v", m.dir, fileState(certErr), fileState(keyErr))
	}
	if !os.IsNotExist(certErr) {
		return fmt.Errorf("read %s: %w", m.CACertPath(), certErr)
	}
	return m.createCA()
}

func (m *Manager) createCA() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return err
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "photobackup"
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"photobackup"},
			CommonName:   fmt.Sprintf("photobackup CA (%s)", host),
		},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// This CA exists to sign one server certificate. Nothing below it ever
		// signs anything else.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("parse new CA certificate: %w", err)
	}

	if err := writeCert(m.CACertPath(), der); err != nil {
		return err
	}
	if err := writeKey(m.caKeyPath(), key); err != nil {
		return err
	}

	m.caCert, m.caDER, m.caKey = cert, der, key
	m.log.Info("generated a TLS certificate authority", "path", m.CACertPath(),
		"not_after", cert.NotAfter, "install_on", "every device that uploads")
	return nil
}

// loadLeaf returns the stored leaf if it is still the right one to serve: signed
// by the CA now in hand, certifying exactly the addresses wanted, and not close
// to expiry.
func (m *Manager) loadLeaf(wanted []string) (issued, bool) {
	certPEM, err := os.ReadFile(m.leafCertPath())
	if err != nil {
		return issued{}, false
	}
	keyPEM, err := os.ReadFile(m.leafKeyPath())
	if err != nil {
		return issued{}, false
	}
	cert, key, err := parsePair(certPEM, keyPEM)
	if err != nil {
		return issued{}, false
	}
	if err := cert.CheckSignatureFrom(m.caCert); err != nil {
		return issued{}, false
	}
	if !slices.Equal(sansOf(cert), wanted) {
		return issued{}, false
	}
	if time.Until(cert.NotAfter) <= renewBefore {
		return issued{}, false
	}

	return issued{
		cert: &tls.Certificate{Certificate: [][]byte{cert.Raw, m.caDER}, PrivateKey: key, Leaf: cert},
		end:  cert.NotAfter,
		sans: wanted,
	}, true
}

func (m *Manager) issueLeaf(wanted []string) (issued, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return issued{}, fmt.Errorf("generate server key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return issued{}, err
	}

	dnsNames, ips := splitSANs(wanted)
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"photobackup"},
			// iOS ignores the common name entirely and matches on SANs, so this
			// is a label for whoever reads the certificate, nothing more.
			CommonName: "photod",
		},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(leafLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, m.caCert, &key.PublicKey, m.caKey)
	if err != nil {
		return issued{}, fmt.Errorf("create server certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return issued{}, fmt.Errorf("parse new server certificate: %w", err)
	}

	if err := writeCert(m.leafCertPath(), der); err != nil {
		return issued{}, err
	}
	if err := writeKey(m.leafKeyPath(), key); err != nil {
		return issued{}, err
	}

	return issued{
		// The CA travels with the leaf. A client that already trusts it does not
		// need it sent, but one debugging with `openssl s_client` gets a
		// complete chain to look at.
		cert: &tls.Certificate{Certificate: [][]byte{der, m.caDER}, PrivateKey: key, Leaf: cert},
		end:  cert.NotAfter,
		sans: wanted,
	}, nil
}

// wantedSANs is every name and address this server should be reachable at,
// sorted so the set can be compared for equality.
//
// Deliberately not shared with discovery.localAddresses. That one requires a
// multicast-capable interface, which is correct for mDNS and wrong here: a
// Tailscale TUN device is not multicast-capable, and leaving the Tailscale
// address out of the certificate would break the away-from-home path only.
func wantedSANs(extra []string) ([]string, error) {
	set := map[string]bool{
		"ip:127.0.0.1":  true,
		"ip:::1":        true,
		"dns:localhost": true,
	}

	if host, err := os.Hostname(); err == nil && host != "" {
		host = strings.TrimSuffix(strings.TrimSuffix(host, "."), ".local")
		if host != "" {
			set["dns:"+host] = true
			// The name Bonjour resolves. The phone finds the server by mDNS, and
			// a certificate that omits this cannot be used with the name that
			// discovery hands back.
			set["dns:"+host+".local"] = true
		}
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			// Link-local addresses are only usable with a scope zone that a
			// certificate cannot carry.
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			set["ip:"+ip.String()] = true
		}
	}

	for _, e := range extra {
		if ip := net.ParseIP(e); ip != nil {
			set["ip:"+ip.String()] = true
			continue
		}
		set["dns:"+e] = true
	}

	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out, nil
}

func splitSANs(sans []string) (dnsNames []string, ips []net.IP) {
	for _, s := range sans {
		switch {
		case strings.HasPrefix(s, "ip:"):
			if ip := net.ParseIP(strings.TrimPrefix(s, "ip:")); ip != nil {
				ips = append(ips, ip)
			}
		case strings.HasPrefix(s, "dns:"):
			dnsNames = append(dnsNames, strings.TrimPrefix(s, "dns:"))
		}
	}
	return dnsNames, ips
}

// sansOf reads a certificate's SANs back into the same form wantedSANs produces,
// so the two can be compared directly.
func sansOf(cert *x509.Certificate) []string {
	out := make([]string, 0, len(cert.DNSNames)+len(cert.IPAddresses))
	for _, name := range cert.DNSNames {
		out = append(out, "dns:"+name)
	}
	for _, ip := range cert.IPAddresses {
		out = append(out, "ip:"+ip.String())
	}
	slices.Sort(out)
	return out
}

func normalizeExtras(extras []string) []string {
	var out []string
	for _, e := range extras {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func parsePair(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, errors.New("no PEM certificate found")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("no PEM private key found")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse private key: %w", err)
	}

	// A certificate and a key that do not belong together would fail at the
	// first handshake instead of here, with a far worse message.
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, errors.New("certificate and private key do not match")
	}
	return cert, key, nil
}

func writeCert(path string, der []byte) error {
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// 0644: a certificate is public by definition, and the CA one has to be
	// readable by whoever is copying it to a phone.
	if err := os.WriteFile(path, block, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	// WriteFile does not narrow the mode of a file that already exists.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", filepath.Base(path), err)
	}
	return nil
}

func serialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial, nil
}

func colonHex(sum []byte) string {
	encoded := strings.ToUpper(hex.EncodeToString(sum))
	var b strings.Builder
	for i := 0; i < len(encoded); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(encoded[i : i+2])
	}
	return b.String()
}

func fileState(err error) string {
	if err == nil {
		return "present"
	}
	if os.IsNotExist(err) {
		return "missing"
	}
	return err.Error()
}
