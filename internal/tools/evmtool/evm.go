// Package evmtool implements evm.* tools: EVM-plane checks against the
// JSON-RPC endpoint (RPC/sentry nodes only).
package evmtool

import (
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Register adds all evm.* tools.
func Register(r *toolkit.Registry) {
	r.Register(parityTool{})
	r.Register(syncingTool{})
	r.Register(gasPriceTool{})
	r.Register(txpoolTool{})
	r.Register(chainIDTool{})
}

type parityTool struct{}

func (parityTool) Name() string { return "evm.parity" }
func (parityTool) Desc() string {
	return "Compare eth_blockNumber vs CometBFT height — detects JSON-RPC lag"
}
func (parityTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (parityTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (parityTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	ev, err := c.EVM()
	if err != nil {
		return nil, err
	}
	evmHeight, err := ev.BlockNumber(c)
	if err != nil {
		return nil, err
	}
	var cometHeight int64 = -1
	if cc, err := c.Comet(); err == nil {
		if st, err := cc.Status(c); err == nil {
			cometHeight = st.SyncInfo.LatestBlockHeight
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "evm height:   %d\n", evmHeight)
	fmt.Fprintf(&b, "comet height: %d\n", cometHeight)
	drift := cometHeight - int64(evmHeight)
	if cometHeight >= 0 {
		fmt.Fprintf(&b, "drift:        %d blocks\n", drift)
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"evm_height": evmHeight, "comet_height": cometHeight, "drift": drift,
	}}, nil
}

type syncingTool struct{}

func (syncingTool) Name() string { return "evm.syncing" }
func (syncingTool) Desc() string {
	return "eth_syncing — EVM-side sync progress"
}
func (syncingTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (syncingTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (syncingTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	ev, err := c.EVM()
	if err != nil {
		return nil, err
	}
	s, err := ev.Syncing(c)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return &toolkit.Result{Text: "not syncing", Data: map[string]any{"syncing": false}}, nil
	}
	return &toolkit.Result{Text: fmt.Sprintf("syncing: %v", s), Data: map[string]any{"syncing": s}}, nil
}

type gasPriceTool struct{}

func (gasPriceTool) Name() string { return "evm.gasprice" }
func (gasPriceTool) Desc() string {
	return "Current eth_gasPrice (wei)"
}
func (gasPriceTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (gasPriceTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (gasPriceTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	ev, err := c.EVM()
	if err != nil {
		return nil, err
	}
	gp, err := ev.GasPrice(c)
	if err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: fmt.Sprintf("gas price: %d wei", gp), Data: map[string]any{"gas_price_wei": gp}}, nil
}

type txpoolTool struct{}

func (txpoolTool) Name() string { return "evm.txpool" }
func (txpoolTool) Desc() string {
	return "txpool_status — pending/queued EVM tx counts"
}
func (txpoolTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (txpoolTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (txpoolTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	ev, err := c.EVM()
	if err != nil {
		return nil, err
	}
	st, err := ev.TxPoolStatus(c)
	if err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: fmt.Sprintf("txpool: %v", st), Data: map[string]any{"txpool": st}}, nil
}

type chainIDTool struct{}

func (chainIDTool) Name() string { return "evm.chainid" }
func (chainIDTool) Desc() string {
	return "Sanity-check eth_chainId vs the profile's expected EIP-155 id"
}
func (chainIDTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (chainIDTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (chainIDTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	ev, err := c.EVM()
	if err != nil {
		return nil, err
	}
	id, err := ev.ChainID(c)
	if err != nil {
		return nil, err
	}
	want := c.Profile.EVMChainID
	status := "✓"
	msg := "matches profile"
	if want != 0 && id != want {
		status = "✗"
		msg = fmt.Sprintf("MISMATCH — profile expects %d", want)
	}
	txt := fmt.Sprintf("%s eth_chainId: %d  (%s)\n", status, id, msg)
	return &toolkit.Result{Text: txt, Data: map[string]any{
		"evm_chain_id": id, "expected": want, "match": want == 0 || id == want,
	}}, nil
}
