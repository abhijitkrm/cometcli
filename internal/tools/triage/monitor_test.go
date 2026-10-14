package triage

import (
	"strings"
	"testing"
)

func TestParseMetrics(t *testing.T) {
	in := `# HELP cometduty_window_missed_percent x
cometduty_window_missed_percent{chain_id="c-1",moniker="v"} 2.5
cometduty_consecutive_missed_blocks{chain_id="c-1"} 0
cometduty_node_disk_free_bytes{node="v"} 1.073741824e+10
cometduty_silences_active 1
cometduty_endpoint_sync_blocks_behind{url="a"} 3
cometduty_endpoint_sync_blocks_behind{url="b"} 12
go_goroutines 40
`
	got := ParseMetrics(strings.NewReader(in))
	want := map[string]float64{"monitor.window_missed_pct": 2.5, "monitor.consecutive_missed": 0,
		"monitor.disk_free_gb": 10, "monitor.silences": 1, "monitor.blocks_behind": 12}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("extra signals: %v", got)
	}
}
