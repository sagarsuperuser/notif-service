package observability

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// DBQueryCalls counts pgx query CALLS that complete through a traced pool.
	//
	// Deliberately not called round-trips any more. An audit established it is
	// neither "every query" nor "one per round-trip": pgx routes pool.Ping below
	// the query tracer, out-of-process SQL never touches it, an acquire failure
	// produces no increment at all, and a statement-cache miss is one increment
	// covering two wire exchanges. It is a good measure of application query
	// volume and a bad one for database load.
	//
	// DBQueryCalls counts every query sent to Postgres, labelled by the service
	// that sent it. Divided by the request rate this is statements per request —
	// the quantity that actually sets the database's CPU, and the one that made
	// this system look database-bound when it was not. Seven statements per
	// accepted send at 500 requests/second is 3,500 statements/second; one is
	// 500. Same offered load, same database.
	DBQueryCalls = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "notif_db_query_calls_total",
			Help: "pgx query calls completing through a traced pool, by component and outcome. NOT every statement the database receives: excludes pool.Ping, out-of-process SQL (reconcile CronJob, migrate Job, verify-run), and acquire failures; a statement-cache miss is one increment but two wire round-trips",
		},
		[]string{"component", "outcome"},
	)

	// DBQuerySeconds is time spent inside a query. Compare it against
	// DBPoolAcquireSeconds: if queries are fast but waits are long, the pool is the
	// bottleneck, not the database.
	DBQuerySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "notif_db_query_seconds",
			Help:    "Time spent executing a query",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		},
	)

	// DBPoolConns exposes pgx's own pool counters. The distinction that matters
	// is acquired vs total: when acquired sits at total, callers are queueing
	// for a connection, and the symptom — requests timing out "on the database"
	// — points at the database while the database is idle. That is the shape of
	// the failure this service actually had.
	DBPoolConns = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "notif_db_pool_connections",
			Help: "pgx pool connection counts by state",
		},
		[]string{"component", "state"},
	)

	// DBPoolAcquireSeconds is cumulative time callers spent acquiring a
	// connection, and DBPoolSlowAcquires counts the acquires that found the
	// pool empty and had to wait. Either one rising while DBQuerySeconds stays
	// flat is pool exhaustion, full stop — the database is answering as fast as
	// ever and the queue is in front of it, not behind it.
	DBPoolAcquireSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "notif_db_pool_acquire_seconds_total",
			Help: "Cumulative time spent acquiring a pooled connection",
		},
		[]string{"component"},
	)

	DBPoolSlowAcquires = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "notif_db_pool_slow_acquires_total",
			Help: "Acquires that did not get an immediately-idle connection: pgx counts BOTH waiting on the pool semaphore AND constructing a new connection, so this mixes contention with pool growth and is not a contention figure on its own",
		},
		[]string{"component"},
	)
)

// Labels are "component", not "service": Prometheus Operator reserves `service`
// for the name of the target Kubernetes Service and relabels a colliding
// application label to `exported_service`. The data survives, but every query
// then needs a prefix nobody expects, which is how a correct counter ends up
// behind an empty panel.
//
// QueryTracer counts and times every query a pool issues. It is a pgx
// QueryTracer, so it sees Exec, Query, QueryRow and the implicit BEGIN/COMMIT
// of an explicit transaction — which is the point: a transaction's round-trips
// should be counted at their true cost, not hidden inside one "operation".
type QueryTracer struct {
	Service string
}

type queryStartKey struct{}

func (t *QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryStartKey{}, time.Now())
}

func (t *QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	outcome := "ok"
	if data.Err != nil {
		outcome = "error"
	}
	DBQueryCalls.WithLabelValues(t.Service, outcome).Inc()
	if start, ok := ctx.Value(queryStartKey{}).(time.Time); ok {
		DBQuerySeconds.Observe(time.Since(start).Seconds())
	}
}

// SamplePool publishes pgx's pool statistics until ctx is cancelled. These are
// gauges pgx already maintains; nothing here is derived or estimated.
func SamplePool(ctx context.Context, service string, pool *pgxpool.Pool, every time.Duration) {
	if every <= 0 {
		every = 5 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := pool.Stat()
			DBPoolConns.WithLabelValues(service, "total").Set(float64(s.TotalConns()))
			DBPoolConns.WithLabelValues(service, "acquired").Set(float64(s.AcquiredConns()))
			DBPoolConns.WithLabelValues(service, "idle").Set(float64(s.IdleConns()))
			DBPoolConns.WithLabelValues(service, "max").Set(float64(s.MaxConns()))
			DBPoolConns.WithLabelValues(service, "constructing").Set(float64(s.ConstructingConns()))
			DBPoolAcquireSeconds.WithLabelValues(service).Set(s.AcquireDuration().Seconds())
			DBPoolSlowAcquires.WithLabelValues(service).Set(float64(s.EmptyAcquireCount()))
		}
	}
}

func RegisterDB(reg prometheus.Registerer) {
	reg.MustRegister(DBQueryCalls, DBQuerySeconds, DBPoolConns, DBPoolAcquireSeconds, DBPoolSlowAcquires)
}
