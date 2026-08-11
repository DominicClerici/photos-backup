package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/config"
	"github.com/dominicclerici/photos-backup/server/internal/verify"
)

// confirmWord is what has to be typed to go ahead. A word rather than y/N
// because this is the only subcommand that destroys originals, and the muscle
// memory that answers yes to a prompt should not be enough to fire it.
const confirmWord = "erase"

// runReset erases the archive and starts over, keeping paired devices.
func runReset(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt, for a scripted rebuild")
	dryRun := fs.Bool("dry-run", false, "report what would be erased without erasing it")
	force := fs.Bool("force", false, "proceed even though photod looks like it is running")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	cfg := config.FromEnv()

	a, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	defer a.close()

	// Resolved the way runCA resolves it, so the check inside verify.Reset is
	// looking at the directory photod actually uses.
	tlsDir := cfg.TLSDir
	if tlsDir == "" {
		tlsDir = filepath.Join(a.deps.PhotosRoot, "tls")
	}

	plan, err := verify.Reset(ctx, a.deps, verify.ResetOptions{DryRun: true, TLSDir: tlsDir})
	if err != nil {
		return fail(err)
	}

	printResetPlan(plan, a.deps.PhotosRoot)

	if *dryRun {
		fmt.Println("\ndry run — nothing was erased")
		return exitOK, nil
	}

	// A live daemon would be writing derivatives and accepting uploads into the
	// directories being removed, and its in-flight rows would outlive the
	// truncate. Stopping it is part of the procedure, so refuse rather than
	// race.
	if addr, running := daemonAddr(cfg); running && !*force {
		fmt.Fprintf(os.Stderr, "\nphotod is listening on %s — stop it first:\n", addr)
		fmt.Fprintf(os.Stderr, "    sudo systemctl stop photod\n")
		fmt.Fprintf(os.Stderr, "\nor re-run with --force if that is not this archive's daemon.\n")
		return exitFindings, nil
	}

	if !*yes {
		ok, err := confirm()
		if err != nil {
			return fail(err)
		}
		if !ok {
			fmt.Println("nothing was erased")
			return exitOK, nil
		}
	}

	result, err := verify.Reset(ctx, a.deps, verify.ResetOptions{TLSDir: tlsDir})
	if err != nil {
		return fail(err)
	}

	fmt.Printf("\nerased %d assets, %s in %s\n",
		result.Assets, byteCount(result.Bytes), round(result.Elapsed))
	fmt.Printf("%d paired device(s) kept — their tokens still work\n", result.Devices)
	fmt.Println("\nStart photod and the archive fills again from scratch.")
	return exitOK, nil
}

func printResetPlan(plan verify.ResetResult, root string) {
	fmt.Printf("\nThis erases the archive at %s.\n\n", root)
	fmt.Printf("  %d assets, %s\n", plan.Assets, byteCount(plan.Bytes))
	for _, t := range plan.Targets {
		note := ""
		if !t.Present {
			note = "  (already gone)"
		}
		fmt.Printf("  %-14s %s%s\n", t.What, t.Path, note)
	}
	fmt.Printf("\n  kept: %d paired device(s), the pairing codes, and the CA\n", plan.Devices)
	fmt.Printf("  gone: every original. There is no undo, and reindex cannot\n")
	fmt.Printf("        bring them back — the manifest goes with them.\n")
}

// confirm reads the confirmation word from the terminal.
//
// A closed or redirected stdin reads as EOF, which aborts. That is the right
// default for a command run from a script without --yes: doing nothing is
// recoverable, and erasing the archive is not.
func confirm() (bool, error) {
	fmt.Printf("\nType %s to confirm: ", confirmWord)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Println()
		return false, nil
	}
	return strings.EqualFold(strings.TrimSpace(line), confirmWord), nil
}

// daemonAddr reports whether something is listening where photod would be.
//
// Best effort by design: it proves a socket is accepting connections, not that
// the process behind it is this archive's photod. A false positive costs a
// --force; the check earns that by catching the ordinary mistake of resetting
// while the service is up.
func daemonAddr(cfg config.Config) (string, bool) {
	for _, addr := range []string{cfg.ListenAddr, cfg.PlaintextAddr} {
		if addr == "" {
			continue
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		dialable := net.JoinHostPort(host, port)
		conn, err := net.DialTimeout("tcp", dialable, 500*time.Millisecond)
		if err != nil {
			continue
		}
		conn.Close()
		return dialable, true
	}
	return "", false
}
