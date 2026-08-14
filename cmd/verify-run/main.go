// Command verify-run asserts, against the database, that a load test did
// correct work — not merely a lot of it. Exit code is non-zero if any invariant
// is violated, so it can gate a benchmark run.
//
// See internal/verify for what is checked and why each check matters.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"notif/internal/verify"
)

func main() {
	var (
		dsn    = flag.String("dsn", os.Getenv("DB_DSN"), "Postgres DSN")
		since  = flag.Duration("since", time.Hour, "only consider messages created within this window")
		maxDay = flag.Int("max-per-day", 0, "the per-recipient daily cap the run was configured with; 0 skips the cap checks")
		stale  = flag.Duration("claim-stale-after", 2*time.Minute, "how long a message may sit in processing before it counts as stuck")
		asJSON = flag.Bool("json", false, "emit JSON")
	)
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "verify-run: -dsn or DB_DSN is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-run: connect: %v\n", err)
		os.Exit(2)
	}
	defer pool.Close()

	cutoff := time.Now().Add(-*since)
	opts := verify.Options{Since: cutoff, MaxPerDay: *maxDay, ClaimStaleAfter: *stale}

	results, err := verify.Run(ctx, pool, verify.Checks(opts))
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-run: %v\n", err)
		os.Exit(2)
	}
	summary, err := verify.Summarize(ctx, pool, cutoff)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-run: summarize: %v\n", err)
		os.Exit(2)
	}

	failed := 0
	for _, r := range results {
		if !r.OK {
			failed++
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"window":     since.String(),
			"messages":   summary,
			"invariants": results,
			"ok":         failed == 0,
		})
	} else {
		fmt.Printf("messages in the last %s: %d total — %d delivered, %d submitted, %d suppressed, %d failed\n\n",
			*since, summary.Total, summary.Delivered, summary.Submitted, summary.Suppressed, summary.Failed)
		for _, r := range results {
			mark := "PASS"
			if !r.OK {
				mark = "FAIL"
			}
			fmt.Printf("[%s] %s\n", mark, r.Name)
			if !r.OK {
				fmt.Printf("       %d violations; e.g. %s\n", r.Violations, r.Sample)
				fmt.Printf("       why it matters: %s\n", r.Why)
			}
		}
		fmt.Println()
		if failed == 0 {
			fmt.Printf("all %d invariants hold\n", len(results))
		} else {
			fmt.Printf("%d of %d invariants VIOLATED — this run's throughput does not describe correct work\n",
				failed, len(results))
		}
	}

	if failed > 0 {
		os.Exit(1)
	}
}
