package node

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

const badAppToml = `
[api]
enable = true
address = "tcp://0.0.0.0:1317"

[grpc]
enable = true
address = "0.0.0.0:9090"

[json-rpc]
enable = true
address = "0.0.0.0:8545"
`

const goodAppToml = `
[api]
enable = false

[grpc]
enable = true
address = "localhost:9090"

[json-rpc]
enable = false
`

func ctxFor(t *testing.T, role string) *toolkit.Context {
	return &toolkit.Context{
		Context: context.Background(),
		Out:     io.Discard,
		Profile: &config.Profile{Name: "t", Role: role},
	}
}

func TestLintValidatorBad(t *testing.T) {
	res := lintConfig(ctxFor(t, "validator"), "app.toml", badAppToml)
	crits, _ := res.Data["critical"].(int)
	if crits < 3 {
		t.Fatalf("expected >=3 critical findings, got %d\n%s", crits, res.Text)
	}
	if !strings.Contains(res.Text, "json-rpc") {
		t.Fatal("expected json-rpc finding")
	}
}

func TestLintRPCNodeOK(t *testing.T) {
	// On rpc nodes, exposure is not flagged critical.
	res := lintConfig(ctxFor(t, "rpc"), "app.toml", badAppToml)
	crits, _ := res.Data["critical"].(int)
	if crits != 0 {
		t.Fatalf("rpc node should not flag exposure critical, got %d", crits)
	}
}

func TestLintGood(t *testing.T) {
	res := lintConfig(ctxFor(t, "validator"), "app.toml", goodAppToml)
	crits, _ := res.Data["critical"].(int)
	if crits != 0 {
		t.Fatalf("good config flagged: %s", res.Text)
	}
}
