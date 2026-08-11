package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/config"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/devices"
	"github.com/dominicclerici/photos-backup/server/internal/tlsca"
)

// runPair mints a pairing code.
//
// It writes to the database rather than asking photod for one, so pairing works
// whether or not the daemon is up, and there is no admin endpoint to protect.
// Being able to create a credential is exactly the authority that filesystem
// access to the database already carries.
func runPair(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	ttl := fs.Duration("ttl", devices.DefaultCodeTTL, "how long the code stands")
	label := fs.String("label", "", "note stored with the code, for remembering what it was for")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	cfg := config.FromEnv()
	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail(fmt.Errorf("open database: %w", err))
	}
	defer store.Close()
	if err := store.Migrate(); err != nil {
		return fail(fmt.Errorf("migrate: %w", err))
	}

	paired := devices.New(store.Pool())
	code, expiresAt, err := paired.CreateCode(ctx, *ttl, *label)
	if err != nil {
		return fail(err)
	}
	if _, err := paired.SweepCodes(ctx); err != nil {
		// Housekeeping. A code that could not be tidied away is inert anyway.
		fmt.Fprintf(os.Stderr, "note: could not sweep old pairing codes: %v\n", err)
	}

	fmt.Printf("\n  pairing code:  %s\n\n", devices.FormatCode(code))
	fmt.Printf("  good for %s, until %s\n", round(time.Until(expiresAt)), expiresAt.Local().Format("15:04:05"))
	fmt.Printf("  single use — redeeming it is what mints the device's token\n\n")
	fmt.Printf("  Enter it in the app under Pairing. If the app cannot reach this\n")
	fmt.Printf("  server over HTTPS yet, install the CA first: photobackup ca\n\n")
	return exitOK, nil
}

// runDevices lists or revokes paired devices.
func runDevices(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("devices", flag.ExitOnError)
	revoke := fs.String("revoke", "", "device id to unpair; its token stops working immediately")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	cfg := config.FromEnv()
	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail(fmt.Errorf("open database: %w", err))
	}
	defer store.Close()
	if err := store.Migrate(); err != nil {
		return fail(fmt.Errorf("migrate: %w", err))
	}
	paired := devices.New(store.Pool())

	if *revoke != "" {
		d, err := paired.Revoke(ctx, strings.TrimSpace(*revoke))
		if errors.Is(err, devices.ErrNoDevice) {
			return exitFindings, fmt.Errorf("no device with id %s", *revoke)
		}
		if err != nil {
			return fail(err)
		}
		fmt.Printf("unpaired %s (%s)\n", d.Name, d.ID)
		fmt.Printf("\nThe photos it already delivered are untouched — revoking a token\n")
		fmt.Printf("removes write access, never anything from the archive.\n")
		return exitOK, nil
	}

	list, err := paired.List(ctx)
	if err != nil {
		return fail(err)
	}
	if len(list) == 0 {
		fmt.Printf("no devices are paired\n\nRun `photobackup pair` to mint a code.\n")
		return exitOK, nil
	}

	fmt.Printf("%-38s %-20s %-12s %-20s %s\n", "ID", "NAME", "PAIRED", "LAST SEEN", "STATE")
	for _, d := range list {
		state := "active"
		if d.Revoked() {
			state = "revoked " + d.RevokedAt.Local().Format("2006-01-02")
		}
		fmt.Printf("%-38s %-20s %-12s %-20s %s\n",
			d.ID, truncate(d.Name, 20), d.CreatedAt.Local().Format("2006-01-02"),
			since(d.LastSeenAt), state)
	}
	return exitOK, nil
}

// runCA reports where the certificate authority is and how to get it onto a
// phone, and can serve it for exactly as long as that takes.
func runCA(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	serve := fs.Bool("serve", false, "serve the CA over plain HTTP until it has been fetched once, then stop")
	addr := fs.String("addr", ":8789", "address to serve it on")
	export := fs.String("export", "", "write a copy of the CA certificate to this path")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	cfg := config.FromEnv()
	root, err := filepath.Abs(cfg.PhotosRoot)
	if err != nil {
		return fail(err)
	}
	tlsDir := cfg.TLSDir
	if tlsDir == "" {
		tlsDir = filepath.Join(root, "tls")
	}

	// Opening the manager rather than reading the file, so this reports the same
	// certificate photod would serve and says so about the same address set.
	certs, err := tlsca.Open(tlsDir, cfg.TLSExtraSANs, slog.New(slog.DiscardHandler))
	if err != nil {
		return fail(err)
	}
	sha256Sum, sha1Sum := certs.Fingerprints()

	fmt.Printf("\n  CA certificate:  %s\n", certs.CACertPath())
	fmt.Printf("  SHA-256:         %s\n", sha256Sum)
	fmt.Printf("  SHA-1:           %s\n\n", sha1Sum)
	fmt.Printf("  the server certificate it signs currently covers:\n")
	for _, san := range certs.SANs() {
		name := strings.TrimPrefix(strings.TrimPrefix(san, "dns:"), "ip:")
		fmt.Printf("    %s\n", name)
	}
	fmt.Printf("\n  A phone dialling anything not on that list will refuse the\n")
	fmt.Printf("  connection. Add it with TLS_EXTRA_SANS.\n")
	fmt.Printf("\n  expires %s, reissued automatically\n\n", certs.NotAfter().Local().Format("2006-01-02"))

	if *export != "" {
		if err := os.WriteFile(*export, certs.CACertPEM(), 0o644); err != nil {
			return fail(fmt.Errorf("write %s: %w", *export, err))
		}
		fmt.Printf("  wrote a copy to %s\n\n", *export)
	}

	if !*serve {
		fmt.Printf("  To install it on the iPhone, run this again with --serve.\n\n")
		return exitOK, nil
	}
	return serveCA(ctx, certs, *addr, sha256Sum)
}

// serveCA hands the CA certificate to one client and shuts down.
//
// Deliberately narrow. The obvious alternative — pointing a general-purpose file
// server at the TLS directory — would publish ca.key alongside it, which is the
// one file in this project that must never leave the machine. This serves a
// single certificate on a port that closes behind it.
//
// It is plain HTTP, which it has to be: the phone cannot validate this server's
// HTTPS until it holds the very file being fetched. That is why the fingerprint
// is printed. iOS shows the certificate's SHA-256 in the profile install dialog,
// and comparing the two is what rules out having been handed somebody else's CA
// on the way — a substitution that would otherwise install a trusted root.
func serveCA(ctx context.Context, certs *tlsca.Manager, addr, fingerprint string) (int, error) {
	pemBytes := certs.CACertPEM()
	fetched := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The content type is what makes Safari offer to install a profile
		// rather than show the certificate as text.
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="photobackup-ca.crt"`)
		if _, err := w.Write(pemBytes); err != nil {
			return
		}
		select {
		case fetched <- r.RemoteAddr:
		default:
		}
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fail(fmt.Errorf("listen on %s: %w", addr, err))
	}

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	fmt.Printf("  Open one of these in Safari on the phone:\n")
	for _, host := range hostsFor(certs.SANs()) {
		fmt.Printf("    http://%s:%s/\n", host, port)
	}
	fmt.Printf("\n  Then: Settings > Profile Downloaded > Install, and\n")
	fmt.Printf("        Settings > General > About > Certificate Trust Settings,\n")
	fmt.Printf("        and switch photobackup on. Both steps are required —\n")
	fmt.Printf("        installing without trusting changes nothing.\n\n")
	fmt.Printf("  Check the SHA-256 in the install dialog matches:\n    %s\n\n", fingerprint)
	fmt.Printf("  waiting for one download (ctrl-c to stop)...\n")

	errs := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return fail(err)
	case from := <-fetched:
		host, _, _ := net.SplitHostPort(from)
		fmt.Printf("\n  sent to %s. Verify the fingerprint before trusting it.\n\n", host)
	case <-ctx.Done():
		fmt.Printf("\n  stopped without serving it.\n\n")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return exitOK, nil
}

// hostsFor picks the addresses worth printing: the routable ones, since the
// phone is not on loopback.
func hostsFor(sans []string) []string {
	var out []string
	for _, san := range sans {
		addr, ok := strings.CutPrefix(san, "ip:")
		if !ok {
			continue
		}
		ip := net.ParseIP(addr)
		if ip == nil || ip.IsLoopback() || ip.To4() == nil {
			continue
		}
		out = append(out, addr)
	}
	if len(out) == 0 {
		out = []string{"<this machine's address>"}
	}
	return out
}

func since(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return round(time.Since(*t)) + " ago"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
