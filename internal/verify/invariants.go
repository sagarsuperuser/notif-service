// Package verify checks the invariants a load test is supposed to leave
// intact.
//
// A throughput number on its own says nothing. "12,000 sends per second" is
// easy to reach if some of them are dropped, double-sent, or billed twice to a
// recipient's daily cap — and none of those failures shows up in a latency
// histogram. These checks read the database after a run and assert the
// properties the system claims to guarantee, so a benchmark result describes
// correct work rather than traffic.
package verify

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Invariant is one property that must hold, expressed as a query returning the
// number of violations and a sample violation to make a failure diagnosable
// without a second query.
type Invariant struct {
	Name  string
	Why   string
	Query string
	Args  []any
}

type Result struct {
	Name       string `json:"name"`
	Violations int64  `json:"violations"`
	Sample     string `json:"sample,omitempty"`
	Why        string `json:"why"`
	OK         bool   `json:"ok"`
}

type Options struct {
	// Since bounds every check to messages created in this window.
	Since time.Time
	// MaxPerDay is the per-recipient cap the run was configured with. Zero
	// skips the cap checks, since without knowing the configured value there is
	// nothing to compare against.
	MaxPerDay int
	// ClaimStaleAfter is how long a message may sit in processing before it
	// counts as stuck. Match the worker's setting.
	ClaimStaleAfter time.Duration
}

func Checks(o Options) []Invariant {
	checks := []Invariant{
		{
			Name: "no recipient was sent the same message twice",
			Why: "the worker's claim is the only thing between an SQS redelivery and a second SMS. " +
				"More than one successful provider attempt for a message means the claim let two workers through.",
			Query: `
				WITH offenders AS (
					SELECT a.message_id, count(*) AS n
					  FROM provider_attempts a
					  JOIN messages m ON m.id = a.message_id
					 WHERE m.created_at >= $1
					   AND a.provider_msg_id IS NOT NULL
					 GROUP BY a.message_id
					HAVING count(*) > 1
				)
				SELECT count(*), coalesce(max(message_id || ' has ' || n || ' successful attempts'), '') FROM offenders`,
			Args: []any{o.Since},
		},
		{
			Name: "every sent message has a provider id",
			Why: "a message marked submitted or delivered without a provider id can never be matched to its " +
				"delivery callback, so its outcome is permanently unknown.",
			Query: `
				SELECT count(*), coalesce(max(id), '')
				  FROM messages
				 WHERE created_at >= $1
				   AND state IN ('submitted','delivered')
				   AND (provider_msg_id IS NULL OR provider_msg_id = '')`,
			Args: []any{o.Since},
		},
		{
			Name: "no message is stuck mid-flight",
			Why: "a message left in processing past the stale window was claimed by a worker that never finished " +
				"and never released it — a send nothing will retry.",
			Query: `
				SELECT count(*), coalesce(max(id || ' stuck since ' || updated_at::text), '')
				  FROM messages
				 WHERE created_at >= $1
				   AND state = 'processing'
				   AND updated_at < now() - $2::interval`,
			Args: []any{o.Since, o.ClaimStaleAfter},
		},
		{
			Name: "no message was left queued",
			Why: "a queued message is one the API accepted and no worker picked up — a silent drop. Run this " +
				"after the queue has drained, or it reports work still legitimately in flight.",
			Query: `
				SELECT count(*), coalesce(max(id), '')
				  FROM messages
				 WHERE created_at >= $1 AND state = 'queued'`,
			Args: []any{o.Since},
		},
		{
			Name: "idempotency keys are unique per tenant",
			Why: "two rows sharing a key would mean one request was accepted twice. The unique constraint makes " +
				"that impossible; checking it proves the constraint exists on the database under test and not " +
				"only in the schema file.",
			Query: `
				WITH dupes AS (
					SELECT tenant_id, idempotency_key, count(*) AS n
					  FROM messages WHERE created_at >= $1
					 GROUP BY tenant_id, idempotency_key HAVING count(*) > 1
				)
				SELECT count(*), coalesce(max(tenant_id || '/' || idempotency_key || ' x' || n), '') FROM dupes`,
			Args: []any{o.Since},
		},
		{
			Name: "suppressed messages were never sent",
			Why: "a suppressed message is one the recipient opted out of, or that exceeded their cap. A successful " +
				"attempt against it means an SMS went out that the system had decided not to send — a compliance " +
				"failure, not a performance one.",
			Query: `
				SELECT count(*), coalesce(max(m.id), '')
				  FROM messages m
				 WHERE m.created_at >= $1
				   AND m.state = 'suppressed'
				   AND EXISTS (SELECT 1 FROM provider_attempts a
				                WHERE a.message_id = m.id AND a.provider_msg_id IS NOT NULL)`,
			Args: []any{o.Since},
		},
	}

	if o.MaxPerDay > 0 {
		checks = append(checks,
			Invariant{
				Name: "no recipient exceeded their daily cap",
				Why: "the cap is enforced by a conditional increment that must never overshoot. A counter above " +
					"the configured maximum means concurrent sends raced past it — the exact failure the old " +
					"increment-then-compensate transaction could produce under load.",
				Query: `
					SELECT count(*), coalesce(max(tenant_id || '/' || phone || ' = ' || count), '')
					  FROM send_caps_daily WHERE count > $1`,
				Args: []any{o.MaxPerDay},
			},
			Invariant{
				Name: "the cap counter matches the sends that consumed it",
				Why: "the counter and the accepted message are written by the same statement, so they cannot " +
					"disagree unless that statement is wrong. This catches an increment with no send, and a " +
					"send with no increment.",
				Query: `
					WITH accepted AS (
						SELECT tenant_id, to_phone, created_at::date AS day, count(*) AS n
						  FROM messages
						 WHERE created_at >= $1 AND state <> 'suppressed'
						 GROUP BY 1,2,3
					), mismatched AS (
						SELECT a.tenant_id, a.to_phone, a.n, c.count
						  FROM accepted a
						  JOIN send_caps_daily c
						    ON c.tenant_id = a.tenant_id AND c.phone = a.to_phone AND c.day = a.day
						 WHERE c.count <> a.n
					)
					SELECT count(*), coalesce(max(tenant_id || '/' || to_phone || ': ' || n || ' sends vs cap ' || count), '')
					  FROM mismatched`,
				Args: []any{o.Since},
			},
		)
	}

	return checks
}

// Run executes every check and returns one result per invariant, in order.
func Run(ctx context.Context, pool *pgxpool.Pool, checks []Invariant) ([]Result, error) {
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		var n int64
		var sample string
		if err := pool.QueryRow(ctx, c.Query, c.Args...).Scan(&n, &sample); err != nil {
			return nil, &CheckError{Name: c.Name, Err: err}
		}
		results = append(results, Result{
			Name: c.Name, Violations: n, Sample: sample, Why: c.Why, OK: n == 0,
		})
	}
	return results, nil
}

type CheckError struct {
	Name string
	Err  error
}

func (e *CheckError) Error() string { return "invariant " + e.Name + ": " + e.Err.Error() }
func (e *CheckError) Unwrap() error { return e.Err }

// Summary counts the messages a run produced, for context alongside the
// invariant results.
type Summary struct {
	Total      int64 `json:"total"`
	Delivered  int64 `json:"delivered"`
	Submitted  int64 `json:"submitted"`
	Suppressed int64 `json:"suppressed"`
	Failed     int64 `json:"failed"`
}

func Summarize(ctx context.Context, pool *pgxpool.Pool, since time.Time) (Summary, error) {
	var s Summary
	err := pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE state = 'delivered'),
		       count(*) FILTER (WHERE state = 'submitted'),
		       count(*) FILTER (WHERE state = 'suppressed'),
		       count(*) FILTER (WHERE state = 'failed')
		  FROM messages WHERE created_at >= $1`, since).
		Scan(&s.Total, &s.Delivered, &s.Submitted, &s.Suppressed, &s.Failed)
	return s, err
}
