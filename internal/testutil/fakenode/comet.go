package fakenode

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	cmted25519 "github.com/cometbft/cometbft/crypto/ed25519"
	cmtjson "github.com/cometbft/cometbft/libs/json"
	"github.com/cometbft/cometbft/p2p"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cometbft/cometbft/types"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"
)

// Comet is a fake CometBFT JSON-RPC endpoint for a simulated chain the
// test drives: height and block times advance as the test says, the node
// can be catching up, and our validator signs (or not) per height.
type Comet struct {
	URL string

	mu         sync.Mutex
	Height     int64
	BlockTime  time.Duration // spacing of block times
	Genesis    time.Time     // time of block 1
	CatchingUp bool
	// LatestTimeOverride, when set, is the latest block's time (a stalled
	// or far-behind node).
	LatestTimeOverride *time.Time
	// Signs reports whether our validator signed height h (default: yes).
	Signs func(h int64) bool
	// InSet is whether our validator is in the active set.
	InSet bool
	Power int64
	Peers int
}

// ConsPub is the test validator's consensus key (ed25519, fixed seed).
func ConsPub() ed25519.PublicKey {
	return ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
}

// ConsAddr is the CometBFT address of ConsPub (sha256(pub)[:20]).
func ConsAddr() []byte {
	s := sha256.Sum256(ConsPub())
	return s[:20]
}

// ConsPubAny is ConsPub as the staking module stores it.
func ConsPubAny() *anypb.Any {
	var v []byte
	v = protowire.AppendTag(v, 1, protowire.BytesType)
	v = protowire.AppendBytes(v, ConsPub())
	return &anypb.Any{TypeUrl: "/cosmos.crypto.ed25519.PubKey", Value: v}
}

// StartComet runs the fake RPC for the test's lifetime.
func StartComet(t testing.TB) *Comet {
	t.Helper()
	c := &Comet{Height: 1000, BlockTime: time.Second, InSet: true, Power: 10, Peers: 3}
	c.Genesis = time.Now().Add(-time.Duration(c.Height) * c.BlockTime)
	srv := httptest.NewServer(http.HandlerFunc(c.handle))
	t.Cleanup(srv.Close)
	c.URL = srv.URL
	return c
}

// Set mutates the chain under its lock.
func (c *Comet) Set(fn func(c *Comet)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// Advance adds n blocks (time moves with them).
func (c *Comet) Advance(n int64) { c.Set(func(c *Comet) { c.Height += n }) }

func (c *Comet) timeAt(h int64) time.Time {
	if h == c.Height && c.LatestTimeOverride != nil {
		return *c.LatestTimeOverride
	}
	return c.Genesis.Add(time.Duration(h) * c.BlockTime)
}

func (c *Comet) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params map[string]any  `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	c.mu.Lock()
	res, err := c.result(req.Method, req.Params)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":%q,"data":%q}}`, req.ID, err.Error(), err.Error())
		return
	}
	raw, _ := cmtjson.Marshal(res)
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, raw)
}

func (c *Comet) heightParam(p map[string]any) int64 {
	switch v := p["height"].(type) {
	case string:
		h, _ := strconv.ParseInt(v, 10, 64)
		return h
	case float64:
		return int64(v)
	}
	return c.Height
}

func (c *Comet) result(method string, p map[string]any) (any, error) {
	addr := ConsAddr()
	switch method {
	case "health":
		return &coretypes.ResultHealth{}, nil
	case "status":
		power := int64(0)
		if c.InSet {
			power = c.Power
		}
		return &coretypes.ResultStatus{
			NodeInfo:      p2p.DefaultNodeInfo{Network: "primium-1", Version: "0.39.3", Moniker: "validator-01"},
			SyncInfo:      coretypes.SyncInfo{LatestBlockHeight: c.Height, LatestBlockTime: c.timeAt(c.Height), CatchingUp: c.CatchingUp},
			ValidatorInfo: coretypes.ValidatorInfo{Address: addr, PubKey: cmted25519.PubKey(ConsPub()), VotingPower: power},
		}, nil
	case "net_info":
		return &coretypes.ResultNetInfo{Listening: true, NPeers: c.Peers}, nil
	case "commit":
		h := c.heightParam(p)
		if h > c.Height {
			return nil, fmt.Errorf("height %d must be less than or equal to the current blockchain height %d", h, c.Height)
		}
		flag := types.BlockIDFlagAbsent
		if c.InSet && (c.Signs == nil || c.Signs(h)) {
			flag = types.BlockIDFlagCommit
		}
		sigs := []types.CommitSig{{BlockIDFlag: types.BlockIDFlagCommit, ValidatorAddress: make([]byte, 20), Timestamp: c.timeAt(h), Signature: make([]byte, 64)}}
		if c.InSet {
			cs := types.CommitSig{BlockIDFlag: flag, ValidatorAddress: addr, Timestamp: c.timeAt(h)}
			if flag == types.BlockIDFlagCommit {
				cs.Signature = make([]byte, 64)
			} else {
				cs.ValidatorAddress, cs.Timestamp = nil, time.Time{}
			}
			sigs = append(sigs, cs)
		}
		hdr := &types.Header{ChainID: "primium-1", Height: h, Time: c.timeAt(h)}
		return coretypes.NewResultCommit(hdr, &types.Commit{Height: h, Signatures: sigs}, true), nil
	case "validators":
		vals := []*types.Validator{types.NewValidator(cmted25519.GenPrivKeyFromSecret([]byte("other")).PubKey(), 100)}
		if c.InSet {
			vals = append(vals, types.NewValidator(cmted25519.PubKey(ConsPub()), c.Power))
		}
		return &coretypes.ResultValidators{BlockHeight: c.heightParam(p), Validators: vals, Count: len(vals), Total: len(vals)}, nil
	}
	return nil, fmt.Errorf("method %s not found", method)
}
