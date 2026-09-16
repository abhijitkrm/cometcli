package node

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// consensusTool dumps the live consensus state — the tool to reach for when
// diagnosing a halt: shows height/round/step and which validators have voted.
// RoundState arrives as raw JSON (structure varies by cometbft version), so
// it is decoded defensively.
type consensusTool struct{}

func (consensusTool) Name() string { return "node.consensus" }
func (consensusTool) Desc() string {
	return "Dump consensus state: height/round/step + validator votes"
}
func (consensusTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (consensusTool) Tier() toolkit.Tier     { return toolkit.TierDiagnose }

func (consensusTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	cc, err := c.Comet()
	if err != nil {
		return nil, err
	}
	cs, err := cc.DumpConsensusState(c)
	if err != nil {
		return nil, fmt.Errorf("dump consensus state: %w", err)
	}
	var rs map[string]any
	_ = json.Unmarshal(cs.RoundState, &rs)

	var b strings.Builder
	fmt.Fprintf(&b, "height/round/step: %v\n", rs["height_round_step"])
	if v, ok := rs["proposal_block_hash"]; ok {
		fmt.Fprintf(&b, "proposal:          %v\n", v)
	}
	if v, ok := rs["locked_round"]; ok {
		fmt.Fprintf(&b, "locked round:      %v\n", v)
	}
	if votes, ok := rs["height_votes"].([]any); ok {
		fmt.Fprintf(&b, "height votes:\n")
		for _, rv := range votes {
			vm, _ := rv.(map[string]any)
			fmt.Fprintf(&b, "  round %v: prevotes=%v precommits=%v\n",
				vm["round"], vm["prevotes_bit_array"], vm["precommits_bit_array"])
		}
	}
	fmt.Fprintf(&b, "peers reporting:  %d\n", len(cs.Peers))
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"round_state": rs}}, nil
}
