package httpserver

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		slog.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start),
		)
	})
}

// isProbe reports whether a request is a Kubernetes health check rather than
// traffic.
//
// These are excluded from the request counter because they are not requests
// anyone means to measure, and because they arrive on a fixed cadence — a
// readiness probe every 5 seconds and a liveness probe every 10 adds roughly
// 26,000 increments per pod per day. Anyone summing the counter without a path
// filter gets that number folded into their traffic, which is how a callback
// total came out at 601,385 when the callbacks numbered 600,000.
func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

func Metrics(counter *prometheus.CounterVec) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			if isProbe(r.URL.Path) {
				return
			}
			counter.WithLabelValues(routeLabel(r), strconv.Itoa(sw.status)).Inc()
		})
	}
}

func routeLabel(r *http.Request) string {
	route := mux.CurrentRoute(r)
	if route == nil {
		return r.URL.Path
	}
	tpl, err := route.GetPathTemplate()
	if err != nil {
		return r.URL.Path

	}
	return tpl
}
