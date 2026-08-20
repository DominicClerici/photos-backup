package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/dominicclerici/photos-backup/server/internal/config"
	"github.com/dominicclerici/photos-backup/server/internal/db"
)

// runMigrate applies pending schema migrations.
//
// photod migrates on start, so this is not what keeps the schema current; it is
// what lets a deployment apply the new binary's migrations while the service is
// down, and hear about a failure as a failed deploy rather than as a daemon
// that will not come back up.
func runMigrate(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	check := fs.Bool("check", false, "report what is pending and change nothing")
	if err := fs.Parse(args); err != nil {
		return exitUsage, nil
	}

	cfg := config.FromEnv()
	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail(fmt.Errorf("open database: %w", err))
	}
	defer store.Close()

	if *check {
		pending, err := store.PendingMigrations()
		if err != nil {
			return fail(err)
		}
		if len(pending) == 0 {
			fmt.Printf("schema is up to date\n")
			return exitOK, nil
		}
		fmt.Printf("%d migration(s) pending:\n", len(pending))
		for _, name := range pending {
			fmt.Printf("  %s\n", name)
		}
		return exitFindings, nil
	}

	applied, err := store.MigrateUp()
	if err != nil {
		return fail(err)
	}
	if len(applied) == 0 {
		fmt.Printf("schema is up to date\n")
		return exitOK, nil
	}
	fmt.Printf("applied %d migration(s):\n", len(applied))
	for _, name := range applied {
		fmt.Printf("  %s\n", name)
	}
	return exitOK, nil
}
