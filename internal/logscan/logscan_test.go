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

func TestScanIgnoresUsageText(t *testing.T) {
	logs := "Error: While parsing config: toml: expected character ]\nUsage:\n  evmd start [flags]\nFlags:\n" +
		"      --consensus.double_sign_check_height int   how many blocks to look back\n" +
		"      --log_level string   (trace|debug|info|warn|error|fatal|panic|disabled)\n" +
		"  -h, --help   help for start\n"
	r := Scan(logs)
	if r.Counts["double_sign"] != 0 || r.Counts["panic"] != 0 {
		t.Errorf("usage text classified: %v", r.Counts)
	}
	if r.Counts["config_parse"] != 1 || r.Counts["startup_error"] != 1 {
		t.Errorf("startup error missed: %v", r.Counts)
	}
	if r.LastError != "Error: While parsing config: toml: expected character ]" {
		t.Errorf("last error %q", r.LastError)
	}
}

func TestOwnJailLine(t *testing.T) {
	me := "cosmosvalcons1aaa"
	if !OwnJailLine("INF validator jailed validator=cosmosvalcons1aaa", me) || OwnJailLine("INF validator jailed validator=cosmosvalcons1bbb", me) {
		t.Fatal("address filter")
	}
	if !OwnJailLine("INF slashing and jailing validator due to liveness fault height=5", me) {
		t.Fatal("line without an address dropped")
	}
}

func TestRegressionIsNotAPrivvalFault(t *testing.T) {
	r := Scan(`5:58AM ERR failed signing vote err="error signing vote: step regression at height 1317 round 0. Got 2, last step 3" module=consensus`)
	if r.Counts["privval"] != 0 || r.Counts["height_regression"] != 1 {
		t.Fatalf("%v", r.Counts)
	}
}

func TestJailPatternIgnoresStoreKeys(t *testing.T) {
	r := Scan(`6:55AM INF Upgrading IAVL storage for faster queries store_key="KVStoreKey{0x400286ccf0, slashing}" version=1
6:55AM INF validator jailed module=x/staking validator=cosmosvalcons1abc`)
	if r.Counts["jail"] != 1 {
		t.Fatalf("jail = %d", r.Counts["jail"])
	}
}
