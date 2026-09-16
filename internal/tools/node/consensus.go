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

	pick := func(keys ...string) any {
		for _, k := range keys {
			if v, ok := rs[k]; ok {
				return v
			}
		}
		return nil
	}
	// Two shapes exist: the simplified /consensus_state view
	// ("height/round/step", "height_vote_set") and the full internal
	// RoundState that DumpConsensusState returns ("height","round","step",
	// "votes"). Handle both.
	var b strings.Builder
	if hrs := pick("height/round/step"); hrs != nil {
		fmt.Fprintf(&b, "height/round/step: %v\n", hrs)
	} else {
		step := pick("step")
		fmt.Fprintf(&b, "height/round/step: %v/%v/%v (%s)\n",
			pick("height"), pick("round"), step, stepName(step))
	}
	if v := pick("proposal_block_hash"); v != nil {
		fmt.Fprintf(&b, "proposal:          %v\n", v)
	} else if pb, ok := rs["proposal_block"].(map[string]any); ok {
		if bid, ok := pb["block_id"].(map[string]any); ok {
			fmt.Fprintf(&b, "proposal:          %v\n", bid["hash"])
		}
	}
	fmt.Fprintf(&b, "locked round:      %v\n", pick("locked_round"))
	if vals, ok := rs["validators"].(map[string]any); ok {
		if vs, ok := vals["validators"].([]any); ok {
			fmt.Fprintf(&b, "validators:        %d\n", len(vs))
		}
	}
	vset, _ := pick("height_vote_set", "votes").([]any)
	for _, rv := range vset {
		vm, _ := rv.(map[string]any)
		fmt.Fprintf(&b, "  round %v: prevotes=%v precommits=%v\n",
			vm["round"], vm["prevotes_bit_array"], vm["precommits_bit_array"])
	}
	fmt.Fprintf(&b, "peers reporting:  %d\n", len(cs.Peers))
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"round_state": rs}}, nil
}

// stepName maps the consensus RoundStep enum to a label.
func stepName(v any) string {
	names := map[float64]string{
		0: "NewHeight", 1: "NewRound", 2: "Propose", 3: "Prevote",
		4: "PrevoteWait", 5: "Precommit", 6: "PrecommitWait", 7: "Commit",
	}
	f, ok := v.(float64)
	if !ok {
		return "?"
	}
	if s, ok := names[f]; ok {
		return s
	}
	return fmt.Sprintf("step %v", v)
}
