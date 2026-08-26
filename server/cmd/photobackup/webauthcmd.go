package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/code"
	"github.com/dominicclerici/photos-backup/server/internal/config"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/webauth"
)

// openWebAuth is the boilerplate every command here shares.
func openWebAuth(ctx context.Context) (*db.Store, *webauth.Store, error) {
	cfg := config.FromEnv()
	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	if err := store.Migrate(); err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	return store, webauth.New(store.Pool()), nil
}

// runPasskey mints enrollment codes, lists credentials, and revokes them.
//
// Minting writes straight to the database rather than asking photod, for the
// reason `photobackup pair` does: the authority an enrollment code transfers is
// filesystem access to this database, and whoever runs this already has it. A
// code is only a way to carry that authority to a browser.
//
// This is also the bootstrap. A fresh archive has no credential, and requireAuth
// refuses everything — so the only way in is somebody standing at this machine.
// That is the intended shape: the archive is closed until its owner opens it,
// rather than open until somebody claims it.
func runPasskey(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, passkeyUsage)
		return exitUsage, nil
	}

	switch args[0] {
	case "add":
		return runPasskeyAdd(ctx, args[1:])
	case "list":
		return runPasskeyList(ctx, args[1:])
	case "revoke":
		return runPasskeyRevoke(ctx, args[1:])
	default:
		fmt.Fprint(os.Stderr, passkeyUsage)
		return exitUsage, nil
	}
}

const passkeyUsage = `photobackup passkey — the credential the browser signs in with

  passkey add [--ttl 5m] [--label L]   mint a single-use enrollment code
  passkey list                         list registered passkeys
  passkey revoke ID                    withdraw one, and end its sessions
`

func runPasskeyAdd(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("passkey add", flag.ExitOnError)
	ttl := fs.Duration("ttl", webauth.DefaultEnrollTTL, "how long the code stands")
	label := fs.String("label", "", "note stored with the code, for remembering what it was for")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	store, web, err := openWebAuth(ctx)
	if err != nil {
		return fail(err)
	}
	defer store.Close()

	enrollCode, expiresAt, err := web.CreateEnrollment(ctx, *ttl, *label)
	if err != nil {
		return fail(err)
	}
	if _, err := web.SweepEnrollments(ctx); err != nil {
		// Housekeeping. A code that could not be tidied away is inert anyway.
		fmt.Fprintf(os.Stderr, "note: could not sweep old enrollment codes: %v\n", err)
	}

	origin := config.FromEnv().WebOrigin
	if origin == "" {
		origin = "https://<this machine>"
	}

	fmt.Printf("\n  enrollment code:  %s\n\n", code.Format(enrollCode))
	fmt.Printf("  good for %s, until %s\n", round(time.Until(expiresAt)), expiresAt.Local().Format("15:04:05"))
	fmt.Printf("  single use — redeeming it is what registers the passkey\n\n")
	fmt.Printf("  Open %s/signin in a browser on the device you want to\n", origin)
	fmt.Printf("  sign in from, enter the code, and approve the passkey prompt.\n\n")

	existing, err := web.HasPasskey(ctx)
	if err == nil && !existing {
		fmt.Printf("  This is the first passkey on this archive. Once it is registered,\n")
		fmt.Printf("  run `photobackup recovery` and keep the codes somewhere that is not\n")
		fmt.Printf("  this machine — a synced passkey is lost with the account that syncs it.\n\n")
	}
	return exitOK, nil
}

func runPasskeyList(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("passkey list", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	store, web, err := openWebAuth(ctx)
	if err != nil {
		return fail(err)
	}
	defer store.Close()

	list, err := web.Passkeys(ctx)
	if err != nil {
		return fail(err)
	}
	if len(list) == 0 {
		fmt.Printf("no passkeys are registered\n\n")
		fmt.Printf("The archive is closed: nothing can sign in until one is.\n")
		fmt.Printf("Run `photobackup passkey add` to mint an enrollment code.\n")
		return exitOK, nil
	}

	fmt.Printf("%-38s %-14s %-12s %-12s %-16s %s\n",
		"ID", "LABEL", "REGISTERED", "LAST USED", "TRANSPORTS", "STATE")
	for _, p := range list {
		state := "active"
		if p.Revoked() {
			state = "revoked " + p.RevokedAt.Local().Format("2006-01-02")
		}
		fmt.Printf("%-38s %-14s %-12s %-12s %-16s %s\n",
			p.ID, truncate(orDash(p.Label), 14), p.CreatedAt.Local().Format("2006-01-02"),
			since(p.LastUsedAt), truncate(orDash(p.Transports), 16), state)
	}

	if n, err := web.RecoveryRemaining(ctx); err == nil {
		fmt.Printf("\n%d recovery code%s remaining\n", n, plural(n))
		if n == 0 {
			fmt.Printf("Run `photobackup recovery` to mint a set — without one, losing every\n")
			fmt.Printf("passkey means losing the way in.\n")
		}
	}
	return exitOK, nil
}

func runPasskeyRevoke(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("passkey revoke", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}
	if fs.NArg() != 1 {
		fmt.Fprint(os.Stderr, "photobackup passkey revoke ID\n")
		return exitUsage, nil
	}

	store, web, err := openWebAuth(ctx)
	if err != nil {
		return fail(err)
	}
	defer store.Close()

	p, killed, err := web.RevokePasskey(ctx, strings.TrimSpace(fs.Arg(0)))
	if errors.Is(err, webauth.ErrNoSuchPasskey) {
		return exitFindings, fmt.Errorf("no passkey with id %s", fs.Arg(0))
	}
	if err != nil {
		return fail(err)
	}

	fmt.Printf("revoked %s (%s)\n", orDash(p.Label), p.ID)
	if killed > 0 {
		fmt.Printf("ended %d session%s it had open\n", killed, plural(int(killed)))
	}

	// Said plainly rather than left to be discovered: an archive whose last
	// passkey has been revoked is reachable only by recovery code, and finding
	// that out at the sign-in page is worse than finding it out here.
	live, err := web.Passkeys(ctx)
	if err == nil {
		remaining := 0
		for _, other := range live {
			if !other.Revoked() {
				remaining++
			}
		}
		if remaining == 0 {
			n, _ := web.RecoveryRemaining(ctx)
			fmt.Printf("\nNo passkeys remain. The browser can now sign in only with one of the\n")
			fmt.Printf("%d remaining recovery code%s, or after `photobackup passkey add`.\n", n, plural(n))
		}
	}
	return exitOK, nil
}

// runRecovery mints a fresh set of recovery codes, retiring the old one.
//
// The whole set is replaced rather than extended, because a set of recovery
// codes is one credential with ten faces: minting more without retiring the old
// ones would mean a list printed a year ago and long since lost still opens the
// archive.
func runRecovery(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("recovery", flag.ExitOnError)
	count := fs.Int("count", webauth.RecoveryCodeCount, "how many codes to mint")
	yes := fs.Bool("yes", false, "skip the confirmation")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	store, web, err := openWebAuth(ctx)
	if err != nil {
		return fail(err)
	}
	defer store.Close()

	if existing, err := web.RecoveryRemaining(ctx); err == nil && existing > 0 && !*yes {
		fmt.Printf("This archive already has %d unused recovery code%s.\n", existing, plural(existing))
		fmt.Printf("Minting a new set retires every one of them immediately.\n\n")
		fmt.Printf("Re-run with --yes to replace them.\n")
		return exitFindings, nil
	}

	codes, err := web.MintRecovery(ctx, *count)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("\n  %d recovery codes. This is the only time they are shown.\n\n", len(codes))
	for _, c := range codes {
		fmt.Printf("    %s\n", c)
	}
	fmt.Printf("\n  Each opens the archive once, from the sign-in page.\n")
	fmt.Printf("  Keep them somewhere that is not this machine and is not the account\n")
	fmt.Printf("  your passkey syncs through — those are the two things they exist to\n")
	fmt.Printf("  survive the loss of.\n\n")
	return exitOK, nil
}

// runWeb lists the browser sessions that are open, and can end them.
func runWeb(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	revokeAll := fs.Bool("revoke-all", false, "end every open browser session immediately")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	store, web, err := openWebAuth(ctx)
	if err != nil {
		return fail(err)
	}
	defer store.Close()

	if *revokeAll {
		n, err := web.RevokeAll(ctx)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("ended %d session%s\n", n, plural(int(n)))
		fmt.Printf("\nEvery browser has to sign in again. Paired devices are untouched —\n")
		fmt.Printf("use `photobackup devices --revoke ID` for those.\n")
		return exitOK, nil
	}

	list, err := web.Sessions(ctx)
	if err != nil {
		return fail(err)
	}
	if len(list) == 0 {
		fmt.Printf("no browser sessions are open\n")
		return exitOK, nil
	}

	fmt.Printf("%-10s %-20s %-16s %-16s %s\n", "METHOD", "SIGNED IN", "LAST SEEN", "FROM", "ENDS")
	for _, sess := range list {
		fmt.Printf("%-10s %-20s %-16s %-16s %s\n",
			sess.Method,
			sess.CreatedAt.Local().Format("2006-01-02 15:04"),
			round(time.Since(sess.LastSeenAt))+" ago",
			truncate(orDash(sess.CreatedFrom), 16),
			round(time.Until(web.Deadline(sess))))
	}

	if _, err := web.Sweep(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "note: could not sweep dead sessions: %v\n", err)
	}
	return exitOK, nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
