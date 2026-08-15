package main

import (
	"testing"

	"notif/internal/config"
)

// TestProviderIdleConns pins the knob's semantics, because an A/B that varies
// it is only valid if zero means "the real default" rather than "no pool".
func TestProviderIdleConns(t *testing.T) {
	if got := providerIdleConns(config.WorkerConfig{WorkerConcurrency: 100}); got != 120 {
		t.Errorf("unset knob gave %d, want concurrency+20 = 120 — the default must track "+
			"concurrency, since every handler may hold a connection at once", got)
	}
	if got := providerIdleConns(config.WorkerConfig{WorkerConcurrency: 100, ProviderMaxIdleConns: 2}); got != 2 {
		t.Errorf("explicit knob gave %d, want 2 — an experiment reproducing Go's default "+
			"must be able to force exactly that", got)
	}
}
