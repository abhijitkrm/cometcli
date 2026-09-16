//go:build e2e

// Package e2e runs cometcli against a live cosmos-evm node. Requirements:
// a local evmd (or chain binary) running with comet RPC + gRPC reachable.
//
//	Run:  COMETCLI_E2E=1 go test -tags e2e ./test/e2e/...
//	Env:  COMETCLI_E2E_COMET (default tcp://127.0.0.1:26657)
//	      COMETCLI_E2E_GRPC  (default 127.0.0.1:9090)
//	      COMETCLI_E2E_EVM   (default http://127.0.0.1:8545)
package e2e

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func testCtx(t *testing.T) *toolkit.Context {
	t.Helper()
	if os.Getenv("COMETCLI_E2E") == "" {
		t.Skip("set COMETCLI_E2E=1 to run against a live node")
	}
	p := &config.Profile{
		Name: "e2e", ChainID: env("COMETCLI_E2E_CHAIN", "cosmos_262144-1"),
		Role: "rpc", Home: os.ExpandEnv("$HOME/.evmd"), Binary: "evmd",
		Endpoints: config.Endpoints{
			Comet: env("COMETCLI_E2E_COMET", "tcp://127.0.0.1:26657"),
			GRPC:  env("COMETCLI_E2E_GRPC", "127.0.0.1:9090"),
			EVM:   env("COMETCLI_E2E_EVM", "http://127.0.0.1:8545"),
		},
		Transport: config.Transport{Type: "local"},
	}
	return &toolkit.Context{
		Context: context.Background(), Profile: p,
		Cfg: &config.Config{Profiles: map[string]*config.Profile{"e2e": p}},
		Out: io.Discard,
	}
}

func TestStatus(t *testing.T) {
	c := testCtx(t)
	reg := toolkit.NewRegistry()
	tools.RegisterAll(reg)
	tool, _ := reg.Get("node.status")
	res, err := tool.Run(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Data["height"].(int64) <= 0 {
		t.Fatal("height should be > 0 on a live node")
	}
	t.Log(res.Text)
}

func TestSnapshot(t *testing.T) {
	c := testCtx(t)
	s := monitor.Collect(c)
	if !s.Reachable {
		t.Fatalf("node unreachable: %v", s.Errors)
	}
	if s.Height <= 0 {
		t.Fatal("no height")
	}
}

func TestEVMParity(t *testing.T) {
	c := testCtx(t)
	reg := toolkit.NewRegistry()
	tools.RegisterAll(reg)
	tool, _ := reg.Get("evm.parity")
	res, err := tool.Run(c, nil)
	if err != nil {
		t.Skip("no evm endpoint: " + err.Error())
	}
	t.Log(res.Text)
	if d := res.Data["drift"].(int64); d > 100 {
		t.Fatalf("excessive drift: %d", d)
	}
}

func TestChainQueries(t *testing.T) {
	c := testCtx(t)
	reg := toolkit.NewRegistry()
	tools.RegisterAll(reg)
	for _, name := range []string{"chain.params", "chain.pool", "chain.upgrade-plan", "chain.validators"} {
		tool, _ := reg.Get(name)
		ctx, cancel := context.WithTimeout(c, 15*time.Second)
		sub := *c
		sub.Context = ctx
		res, err := tool.Run(&sub, nil)
		cancel()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		t.Logf("%s:\n%s", name, res.Text)
	}
}
