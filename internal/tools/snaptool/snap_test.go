package snaptool

import (
	"strings"
	"testing"
)

const cometCfg = `[p2p]
persistent_peers = ""

#######################################################
###         State Sync Configuration Options        ###
#######################################################
[statesync]
# State sync rapidly bootstraps a new node by discovering, fetching, and restoring a state machine
enable = false

# trust_height and trust_hash are filled from a trusted RPC
rpc_servers = ""
trust_height = 0
trust_hash = ""
trust_period = "168h0m0s"
discovery_time = "15s"
temp_dir = ""

[blocksync]
version = "v0"
`

func TestPatchStatesyncInPlace(t *testing.T) {
	out := patchStatesync(cometCfg, "http://a:26657,http://b:26657", 1200, "ABCD", "112h0m0s")
	for _, want := range []string{
		"enable = true", `rpc_servers = "http://a:26657,http://b:26657"`, "trust_height = 1200",
		`trust_hash = "ABCD"`, `trust_period = "112h0m0s"`,
		`discovery_time = "15s"`, "# trust_height and trust_hash are filled from a trusted RPC", "[blocksync]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(out, "trust_height = ") != 1 || strings.Count(out, "enable = ") != 1 {
		t.Fatalf("duplicated keys:\n%s", out)
	}
	if strings.Contains(out, "enable = false") {
		t.Fatal("old value kept")
	}
	// the [p2p] section's lines are untouched
	if !strings.HasPrefix(out, "[p2p]\npersistent_peers = \"\"\n") {
		t.Fatalf("other sections changed:\n%s", out)
	}
}

func TestPatchStatesyncSectionLastOrMissing(t *testing.T) {
	last := "[p2p]\nx = 1\n\n[statesync]\nenable = false\n"
	out := patchStatesync(last, "a,b", 5, "H", "1h")
	for _, k := range []string{"enable = true", `rpc_servers = "a,b"`, "trust_height = 5", `trust_hash = "H"`, `trust_period = "1h"`} {
		if strings.Count(out, k) != 1 {
			t.Errorf("last section: %q count %d in\n%s", k, strings.Count(out, k), out)
		}
	}
	none := "[p2p]\nx = 1\n"
	out = patchStatesync(none, "a,b", 5, "H", "1h")
	if !strings.Contains(out, "\n[statesync]\nenable = true\n") || !strings.Contains(out, "trust_height = 5") || !strings.HasPrefix(out, "[p2p]\nx = 1\n") {
		t.Fatalf("missing section not appended:\n%s", out)
	}
}

func TestPatchStatesyncExactKeys(t *testing.T) {
	cfg := "[statesync]\nenable_extra = \"keep\"\nenable = false\n[x]\n"
	out := patchStatesync(cfg, "a,b", 1, "H", "1h")
	if !strings.Contains(out, `enable_extra = "keep"`) || strings.Count(out, "enable = true") != 1 {
		t.Fatalf("prefix-matched a different key:\n%s", out)
	}
}
