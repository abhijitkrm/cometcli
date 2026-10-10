package netspec

import (
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

func TestSetTOML(t *testing.T) {
	doc := "# top\nmoniker = \"a\"\n\n[rpc]\n# the address\nladdr = \"tcp://127.0.0.1:26657\"\n\n[p2p]\nseeds = \"\"\n"
	out := SetTOML(doc, "rpc", "laddr", `"tcp://0.0.0.0:26657"`)
	if !strings.Contains(out, "# the address\nladdr = \"tcp://0.0.0.0:26657\"") {
		t.Fatalf("replace keeps comments:\n%s", out)
	}
	out = SetTOML(out, "rpc", "cors_allowed_origins", "[]")
	if !strings.Contains(out, "laddr = \"tcp://0.0.0.0:26657\"\ncors_allowed_origins = []\n\n[p2p]") {
		t.Fatalf("a new key joins its section:\n%s", out)
	}
	out = SetTOML(out, "", "db_backend", `"goleveldb"`)
	if i, j := strings.Index(out, "db_backend"), strings.Index(out, "[rpc]"); i < 0 || i > j {
		t.Fatalf("a top-level key goes before the first section:\n%s", out)
	}
	out = SetTOML(out, "mempool", "type", `"app"`)
	if !strings.HasSuffix(out, "[mempool]\ntype = \"app\"\n") {
		t.Fatalf("a missing section is appended:\n%s", out)
	}
	var m map[string]any
	if _, err := toml.Decode(out, &m); err != nil {
		t.Fatalf("result must stay valid TOML: %v\n%s", err, out)
	}
}

// A node rendered from evmd init's own defaults passes network check for
// its role: the renderer and the checker agree.
func TestRenderedNodePassesCheck(t *testing.T) {
	read := func(f string) string {
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	s := FromEnv(ParseEnv(env))
	for _, role := range []Role{Validator, Archive, RPC} {
		for _, prod := range []bool{false, true} {
			n := NodeParams{Moniker: "n1", ExternalAddress: "203.0.113.7:26656", PersistentPeers: []string{"abc@10.0.0.1:26656"}, Prod: prod}
			patches := s.Patches(role, "v0.7.2", n)
			var cfg, app, client map[string]any
			for _, x := range []struct {
				file string
				dst  *map[string]any
			}{{"config.toml", &cfg}, {"app.toml", &app}, {"client.toml", &client}} {
				out := Apply(read(x.file), x.file, patches)
				if _, err := toml.Decode(out, x.dst); err != nil {
					t.Fatalf("%s/%v: %s doesn't parse: %v", role, prod, x.file, err)
				}
			}
			if f := Evaluate("n1", s.Expectations(role, "v0.7.2", prod), cfg, app, s.Command(role)); len(f) > 0 {
				t.Errorf("%s prod=%v: rendered node fails its own check:\n%s", role, prod, whats(f))
			}
			if client["chain-id"] != "primium-1" || Lookup(cfg, []string{"p2p", "persistent_peers"}) != "abc@10.0.0.1:26656" {
				t.Errorf("%s: client chain-id %v, peers %v", role, client["chain-id"], Lookup(cfg, []string{"p2p", "persistent_peers"}))
			}
		}
	}
}

func TestComposeFile(t *testing.T) {
	s := FromEnv(ParseEnv(env))
	c := Compose{Image: "primium-v0.7.2", Container: "primium-archive", HostHome: "/data/archive", User: "1000:1000",
		Command: s.Command(Archive), Services: Services{API: true, GRPC: true, JSONRPC: true, WS: true}, PortOffset: 50}
	y := c.YAML()
	for _, want := range []string{`"--pruning", "nothing"`, `"--json-rpc.ws-origins=*"`, `"26706:26656"`, `"127.0.0.1:26707:26657"`, `"127.0.0.1:8595:8545"`, "/data/archive:/data/node0/evmd", `user: "1000:1000"`, "stop_grace_period: 60s"} {
		if !strings.Contains(y, want) {
			t.Errorf("compose lacks %s:\n%s", want, y)
		}
	}
	var m map[string]any
	if err := yamlUnmarshal(y, &m); err != nil {
		t.Fatalf("compose isn't valid YAML: %v", err)
	}
}

func yamlUnmarshal(s string, v any) error { return yaml.Unmarshal([]byte(s), v) }
