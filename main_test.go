package main

import (
	"reflect"
	"testing"
	"time"

	m "github.com/plusev-terminal/go-plugin-common/meta"
)

// TestRateLimitsPinned pins the manifest rate-limit table so the declared
// numbers can't silently drift. BMF publishes no limits; this is the courtesy
// rule (1 req/s, burst 5, per IP).
func TestRateLimitsPinned(t *testing.T) {
	targets := (&BMFRatesPlugin{}).GetMeta().Resources.AllowedNetworkTargets
	if len(targets) != 1 {
		t.Fatalf("expected 1 network target, got %d", len(targets))
	}

	tgt := targets[0]
	if tgt.Pattern != "https://www.bundesfinanzministerium.de/*" {
		t.Errorf("pattern = %q, want bundesfinanzministerium.de", tgt.Pattern)
	}

	if len(tgt.Limits) != 1 {
		t.Fatalf("expected 1 limit rule, got %d", len(tgt.Limits))
	}

	want := m.NewRateLimitRule("total", 1, time.Second).WithBurst(5)
	got := tgt.Limits[0]
	if !reflect.DeepEqual(got, want) {
		t.Errorf("limit rule = %+v, want %+v", got, want)
	}

	// Manifest limits declared => legacy per-command limits must be an
	// explicit opt-out (empty, not nil).
	if limits := (&BMFRatesPlugin{}).GetRateLimits(); limits == nil || len(limits) != 0 {
		t.Errorf("GetRateLimits() = %v, want empty non-nil slice", limits)
	}
}
