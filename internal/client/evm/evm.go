// Package evm is a minimal Ethereum JSON-RPC client. Validators never expose
// this endpoint — it is used only against RPC/sentry profiles.
package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Client speaks JSON-RPC 2.0 to an eth endpoint.
type Client struct {
	url string
	hc  *http.Client
	id  atomic.Int64
}

// New creates a client for an http(s):// endpoint.
func New(url string) *Client {
	return &Client{url: url, hc: &http.Client{Timeout: 15 * time.Second}}
}

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Call invokes method with params and decodes into out.
func (c *Client) Call(ctx context.Context, method string, params []any, out any) error {
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(rpcReq{JSONRPC: "2.0", ID: c.id.Add(1), Method: method, Params: params})
	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r rpcResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("%s: rpc error %d: %s", method, r.Error.Code, r.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

func hexToUint64(s string) uint64 {
	n, _ := strconv.ParseUint(s, 0, 64)
	return n
}

// BlockNumber returns the latest EVM block number.
func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	var hex string
	if err := c.Call(ctx, "eth_blockNumber", nil, &hex); err != nil {
		return 0, err
	}
	return hexToUint64(hex), nil
}

// Syncing returns nil if synced, or the sync progress object.
func (c *Client) Syncing(ctx context.Context) (map[string]any, error) {
	var raw json.RawMessage
	err := c.Call(ctx, "eth_syncing", nil, &raw)
	if err != nil {
		return nil, err
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil && !b {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{"raw": string(raw)}, nil
	}
	return m, nil
}

// GasPrice returns the current gas price in wei.
func (c *Client) GasPrice(ctx context.Context) (uint64, error) {
	var hex string
	if err := c.Call(ctx, "eth_gasPrice", nil, &hex); err != nil {
		return 0, err
	}
	return hexToUint64(hex), nil
}

// PeerCount returns net_peerCount.
func (c *Client) PeerCount(ctx context.Context) (uint64, error) {
	var hex string
	if err := c.Call(ctx, "net_peerCount", nil, &hex); err != nil {
		return 0, err
	}
	return hexToUint64(hex), nil
}

// ClientVersion returns web3_clientVersion.
func (c *Client) ClientVersion(ctx context.Context) (string, error) {
	var s string
	err := c.Call(ctx, "web3_clientVersion", nil, &s)
	return s, err
}

// ChainID returns eth_chainId (the EIP-155 id).
func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	var hex string
	if err := c.Call(ctx, "eth_chainId", nil, &hex); err != nil {
		return 0, err
	}
	return hexToUint64(hex), nil
}

// TxPoolStatus returns txpool_status {pending, queued} counts.
func (c *Client) TxPoolStatus(ctx context.Context) (map[string]uint64, error) {
	var m map[string]string
	if err := c.Call(ctx, "txpool_status", nil, &m); err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	for k, v := range m {
		out[k] = hexToUint64(v)
	}
	return out, nil
}
