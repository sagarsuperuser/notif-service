package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
)

// TestMetricsMiddleware_ExcludesHealthProbes guards a counter that was inflated
// by Kubernetes.
//
// Readiness runs every 5 seconds and liveness every 10, so a pod adds roughly
// 26,000 probe requests a day to a counter meant to measure traffic. Summing it
// without a path filter is then wrong by that amount — which is how a total of
// 601,385 was reported for 600,000 callbacks.
func TestMetricsMiddleware_ExcludesHealthProbes(t *testing.T) {
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_requests_total"}, []string{"path", "status"})
	reg := prometheus.NewRegistry()
	if err := reg.Register(counter); err != nil {
		t.Fatalf("register: %v", err)
	}

	r := mux.NewRouter()
	r.Use(Metrics(counter))
	for _, p := range []string{"/healthz", "/readyz", "/v1/real"} {
		r.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	}

	for _, p := range []string{"/healthz", "/healthz", "/readyz", "/readyz", "/readyz", "/v1/real"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}

	total := 0.0
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	if total != 1 {
		t.Errorf("counter recorded %v requests; want 1 — five of the six were health probes "+
			"and must not be counted as traffic", total)
	}
}
