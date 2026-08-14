package observability

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// reserved are label names Prometheus Operator assigns to every scraped series.
//
// A metric shipping its own label with one of these names is not dropped and
// does not error. Prometheus keeps the target's value under the original name
// and moves the application's to `exported_<name>` — verified here against a
// live cluster, where exported_endpoint="/v1/webhooks/twilio/status" held the
// correct 600,000 while endpoint="metrics" held the port name.
//
// So this is a usability failure rather than data loss, and it is worth a gate
// because of how it presents: the counter is right, the scrape succeeds, and a
// query written the obvious way returns nothing. It cost several runs here
// before anyone thought to look under a prefix.
var reserved = map[string]string{
	"endpoint":  "the Service port name the Operator scraped",
	"instance":  "the target address",
	"job":       "the scrape job",
	"namespace": "the target's namespace",
	"pod":       "the target pod",
	"service":   "the target Service",
	"container": "the target container",
}

// TestMetricLabelsAvoidOperatorReservedNames checks every label on every metric
// this package declares.
func TestMetricLabelsAvoidOperatorReservedNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	for _, c := range []prometheus.Collector{
		APIRequests, WebhookRequests, Enqueues, TwilioSend,
		WorkerProcessed, WebhookEvents, WebhookMessageUpdateNotFound,
		DBRoundTrips, DBPoolConns, DBPoolAcquireSeconds, DBPoolEmptyAcquires,
	} {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	// A CounterVec emits nothing until a label set is used, so touch each one
	// to make its labels observable.
	APIRequests.WithLabelValues("/v1/sms/messages", "202")
	WebhookRequests.WithLabelValues("/v1/webhooks/twilio/status", "200")
	DBRoundTrips.WithLabelValues("api", "ok")
	families, err = reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	checked := 0
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				checked++
				if why, bad := reserved[lp.GetName()]; bad {
					t.Errorf("metric %s carries label %q, which Prometheus Operator reserves for %s. "+
						"On scrape the target's value takes that name and this one is moved to "+
						"exported_%s, so a query written the obvious way returns nothing while the "+
						"counter is perfectly correct. Rename it (path, component, route).",
						mf.GetName(), lp.GetName(), why, lp.GetName())
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no labels were examined; the test is not checking what it claims")
	}
	t.Logf("checked %d labels across %d metric families", checked, len(families))
}

// TestRequestCountersUsePath pins the specific rename, so a future edit back to
// "endpoint" fails here rather than in a dashboard nobody is watching.
func TestRequestCountersUsePath(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(WebhookRequests); err != nil {
		t.Fatalf("register: %v", err)
	}
	WebhookRequests.WithLabelValues("/v1/webhooks/twilio/status", "200")

	families, _ := reg.Gather()
	var got []string
	for _, mf := range families {
		if mf.GetName() != "notif_webhook_requests_total" {
			continue
		}
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			got = append(got, lp.GetName())
		}
	}
	if len(got) == 0 {
		t.Fatal("notif_webhook_requests_total not found")
	}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "path") {
		t.Errorf("labels are %q, want one named \"path\"", joined)
	}
	_ = dto.Metric{}
}
