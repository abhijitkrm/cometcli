package evm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func server(t *testing.T, results map[string]string, status int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			w.Write([]byte("<html>bad gateway</html>"))
			return
		}
		var req rpcReq
		json.NewDecoder(r.Body).Decode(&req)
		res, ok := results[req.Method]
		if !ok {
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + res + `}`))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestQuantitiesAndSyncing(t *testing.T) {
	c := server(t, map[string]string{
		"eth_blockNumber": `"0x279c8"`, "eth_gasPrice": `"0x3b9aca00"`, "net_peerCount": `"0x3"`,
		"eth_chainId": `"0x1e241"`, "eth_syncing": `false`, "web3_clientVersion": `"evmd/v0.7.2"`,
		"txpool_status": `{"pending":"0x2","queued":"0x0"}`,
	}, 200)
	ctx := context.Background()
	if n, err := c.BlockNumber(ctx); err != nil || n != 162248 {
		t.Fatalf("block = %d %v", n, err)
	}
	if n, _ := c.GasPrice(ctx); n != 1_000_000_000 {
		t.Fatalf("gas = %d", n)
	}
	if n, _ := c.ChainID(ctx); n != 123457 {
		t.Fatalf("chain id = %d", n)
	}
	if s, err := c.Syncing(ctx); err != nil || s != nil {
		t.Fatalf("syncing false → nil: %v %v", s, err)
	}
	if v, _ := c.ClientVersion(ctx); v != "evmd/v0.7.2" {
		t.Fatalf("version = %q", v)
	}
	if p, err := c.TxPoolStatus(ctx); err != nil || p["pending"] != 2 {
		t.Fatalf("txpool = %v %v", p, err)
	}
	c2 := server(t, map[string]string{"eth_syncing": `{"currentBlock":"0x10","highestBlock":"0x20"}`}, 200)
	if s, err := c2.Syncing(ctx); err != nil || s["highestBlock"] != "0x20" {
		t.Fatalf("syncing object: %v %v", s, err)
	}
}

func TestMalformedAndErrorsAreErrors(t *testing.T) {
	ctx := context.Background()
	c := server(t, map[string]string{"eth_blockNumber": `"162248"`}, 200)
	if _, err := c.BlockNumber(ctx); err == nil || !strings.Contains(err.Error(), "bad quantity") {
		t.Fatalf("decimal string must not parse as a block number: %v", err)
	}
	if _, err := c.GasPrice(ctx); err == nil || !strings.Contains(err.Error(), "method not found") {
		t.Fatalf("rpc error: %v", err)
	}
	c = server(t, nil, 502)
	if _, err := c.BlockNumber(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("http error: %v", err)
	}
}
