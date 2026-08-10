package main

import (
	"fmt"
	"os"
	"time"
)

// interactive reports whether stdout is a terminal.
//
// The in-place progress line is redrawn with carriage returns and an erase
// sequence, which is right in a terminal and garbage everywhere else. Under the
// systemd timer stdout is the journal, and a few thousand escape sequences per
// run is a log nobody can read.
func interactive() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// progressTicker returns a callback that redraws at most a few times a second.
//
// A deep verify of a 6TB archive is an hours-long read, and a tool that prints
// nothing for hours is indistinguishable from a hung one. Rate-limited because
// the alternative — a write per asset — makes the terminal the bottleneck.
func progressTicker() func(done, total int64) {
	if !interactive() {
		return nil
	}

	last := time.Now()
	started := last

	return func(done, total int64) {
		if time.Since(last) < 250*time.Millisecond && done != total {
			return
		}
		last = time.Now()

		elapsed := time.Since(started)
		fmt.Printf("\r\033[K%d/%d assets  %s", done, total, round(elapsed))
		if done > 0 && done < total {
			remaining := time.Duration(float64(elapsed) / float64(done) * float64(total-done))
			fmt.Printf("  about %s left", round(remaining))
		}
	}
}

func reindexTicker() func(done int64) {
	if !interactive() {
		return nil
	}

	last := time.Now()
	return func(done int64) {
		if time.Since(last) < 250*time.Millisecond {
			return
		}
		last = time.Now()
		fmt.Printf("\r\033[K%d lines", done)
	}
}

// clearProgress wipes the in-place line so the summary does not land on top of
// it. A no-op when nothing was drawn.
func clearProgress(active bool) {
	if active && interactive() {
		fmt.Print("\r\033[K")
	}
}

func round(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	default:
		return d.Round(time.Second).String()
	}
}

func byteCount(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
