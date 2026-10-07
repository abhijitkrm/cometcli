package logscan

import "testing"

func TestScanLevelsAndColors(t *testing.T) {
	logs := "\x1b[90m5:30AM\x1b[0m \x1b[32mINF\x1b[0m Completed ABCI Handshake appHash=E3B0C4 appHeight=0 module=consensus\n" +
		"5:31AM INF Stopping peer for error err=EOF module=p2p\n" +
		"5:32AM ERR CONSENSUS FAILURE!!! err=\"wrong Block.Header.AppHash. Expected 1A, got 2B\"\n" +
		"5:33AM INF slashing and jailing validator due to liveness fault height=50\n" +
		"\x1b[90m5:34AM\x1b[0m \x1b[31mERR\x1b[0m UPGRADE \"v2\" NEEDED at height: 100: \n" +
		"I[2026-10-06|21:41:13.000] dial tcp 1.2.3.4:26656: i/o timeout module=p2p\n" +
		"E[2026-10-06|21:42:00.000] failed to write to db: no space left on device\n"
	r := Scan(logs)
	want := map[string]int{"apphash": 1, "panic": 1, "jail": 1, "upgrade_needed": 1, "peer_net": 0, "disk_io": 1}
	for k, v := range want {
		if r.Counts[k] != v {
			t.Errorf("%s = %d, want %d", k, r.Counts[k], v)
		}
	}
	if r.UpgradeName != "v2" {
		t.Errorf("upgrade name %q", r.UpgradeName)
	}
}
