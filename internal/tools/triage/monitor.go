package triage

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// monitorSeries are the cometduty series triage reads from a node's monitor
// endpoint (endpoints.monitor) — what the monitor has seen over time that a
// single triage can't (signing streaks, its own alerts, its silences).
var monitorSeries = map[string]string{
	"cometduty_window_missed_percent":       "monitor.window_missed_pct",
	"cometduty_consecutive_missed_blocks":   "monitor.consecutive_missed",
	"cometduty_node_signer_height":          "monitor.signer_height",
	"cometduty_node_disk_free_bytes":        "monitor.disk_free_gb",
	"cometduty_active_alerts":               "monitor.active_alerts",
	"cometduty_silences_active":             "monitor.silences",
	"cometduty_endpoint_sync_blocks_behind": "monitor.blocks_behind",
}

// collectMonitor reads the Prometheus endpoint of the monitor watching this
// node, when the profile names one. No monitor configured = no signals.
func collectMonitor(c *toolkit.Context, _ *Report, set func(string, any)) error {
	if c.Profile == nil || c.Profile.Endpoints.Monitor == "" {
		return nil
	}
	u := c.Profile.Endpoints.Monitor
	if !strings.Contains(u, "/metrics") {
		u = strings.TrimRight(u, "/") + "/metrics"
	}
	req, err := http.NewRequestWithContext(c, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	hc, err := c.NodeHTTP(u, 8*time.Second)
	if err != nil {
		set("monitor.reachable", false)
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		set("monitor.reachable", false)
		return fmt.Errorf("monitor %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		set("monitor.reachable", false)
		return fmt.Errorf("monitor %s: http %d", u, resp.StatusCode)
	}
	set("monitor.reachable", true)
	for sig, v := range ParseMetrics(resp.Body) {
		set(sig, v)
	}
	return nil
}

// ParseMetrics reads the Prometheus text format and returns the monitor
// signals it knows. Per-node series take the highest value when several
// series match (one sidecar usually watches one node).
func ParseMetrics(r io.Reader) map[string]float64 {
	out := map[string]float64{}
	sc := bufio.NewScanner(io.LimitReader(r, 8<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i > 0 {
			name = line[:i]
		}
		sig, ok := monitorSeries[name]
		if !ok {
			continue
		}
		rest := line[len(name):]
		if i := strings.LastIndex(rest, "}"); i >= 0 {
			rest = rest[i+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		if sig == "monitor.disk_free_gb" {
			v = round2(v / (1 << 30))
		}
		if prev, seen := out[sig]; !seen || v > prev {
			out[sig] = v
		}
	}
	return out
}
