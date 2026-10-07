// Package govtool creates governance proposals and deposits: text,
// software upgrades (with a reachability check on the upgrade height) and
// arbitrary messages. Voting is val.vote; waiting is wait.until
// condition=proposal-status.
package govtool

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	govv1 "cosmossdk.io/api/cosmos/gov/v1"
	upgradev1beta1 "cosmossdk.io/api/cosmos/upgrade/v1beta1"
	"github.com/cosmos/btcutil/bech32"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

// Register adds the gov.* tools.
func Register(r *toolkit.Registry) {
	r.Register(proposeTool{})
	r.Register(depositTool{})
}

// ModuleAddress is a module account's address: sha256(name)[:20].
func ModuleAddress(name, prefix string) (string, error) {
	h := sha256.Sum256([]byte(name))
	conv, err := bech32.ConvertBits(h[:20], 8, 5, true)
	if err != nil {
		return "", err
	}
	return bech32.Encode(prefix, conv)
}

func accountPrefix(acct string) string {
	if i := strings.LastIndex(acct, "1"); i > 0 {
		return acct[:i]
	}
	return "cosmos"
}

var planNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,139}$`)

type proposeTool struct{}

func (proposeTool) Name() string { return "gov.propose" }
func (proposeTool) Desc() string {
	return "Submit a governance proposal: kind=text (title, summary), kind=upgrade (name = the release's upgrade handler name, height or in_blocks; " +
		"refused when the vote can't finish before the height), or kind=messages (JSON array of {\"@type\":…} msgs, authority = the gov module). " +
		"Deposit defaults to the chain's minimum. Returns the proposal id."
}
func (proposeTool) Schema() map[string]any {
	return toolkit.ObjSchema(common.WithTx(map[string]any{
		"kind":      toolkit.Enum("proposal kind", "text", "upgrade", "messages"),
		"title":     toolkit.Str("proposal title"),
		"summary":   toolkit.Str("proposal summary / description"),
		"metadata":  toolkit.Str("metadata (ipfs:// link or JSON); text proposals need metadata or a summary"),
		"deposit":   toolkit.Str("initial deposit, e.g. 10000000stake (default: the chain's min deposit)"),
		"expedited": toolkit.Bool("expedited proposal (shorter voting, higher threshold and deposit)"),
		"name":      toolkit.Str("upgrade: plan name — must match the new binary's upgrade handler"),
		"height":    toolkit.Int("upgrade: absolute halt height"),
		"in_blocks": toolkit.Int("upgrade: halt this many blocks from now (instead of height)"),
		"info":      toolkit.Str("upgrade: plan info (binaries JSON / release URL)"),
		"messages":  toolkit.Str(`messages: JSON array of proposal msgs, e.g. [{"@type":"/cosmos.bank.v1beta1.MsgSend",…}]`),
		"memo":      toolkit.Str("tx memo"),
	}), "kind", "title", "summary")
}
func (proposeTool) Tier() toolkit.Tier { return toolkit.TierOnChain }

// Plan is a validated proposal before signing.
type Plan struct {
	Msg      *govv1.MsgSubmitProposal
	Notes    []string // what the operator should know before approving
	Detail   map[string]string
	Proposer string
}

// Build validates the arguments against the live chain and builds the
// proposal message (no signing).
func Build(c *toolkit.Context, a toolkit.Args, proposer string) (*Plan, error) {
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	pr, err := g.GovV1.Params(c, &govv1.QueryParamsRequest{ParamsType: "voting"})
	if err != nil {
		return nil, fmt.Errorf("gov params: %w", err)
	}
	params := pr.Params
	if params == nil {
		return nil, fmt.Errorf("gov params: empty response")
	}
	title, summary := strings.TrimSpace(a.String("title", "")), strings.TrimSpace(a.String("summary", ""))
	if title == "" || summary == "" {
		return nil, fmt.Errorf("title and summary are required")
	}
	expedited := a.Bool("expedited", false)
	p := &Plan{Proposer: proposer, Detail: map[string]string{"action": "gov-propose", "title": title}}
	msg := &govv1.MsgSubmitProposal{Proposer: proposer, Title: title, Summary: summary, Metadata: a.String("metadata", ""), Expedited: expedited}

	// deposit: explicit, else the minimum for this proposal type
	minDep := params.MinDeposit
	if expedited && len(params.ExpeditedMinDeposit) > 0 {
		minDep = params.ExpeditedMinDeposit
	}
	if d := strings.TrimSpace(a.String("deposit", "")); d != "" {
		coins, err := parseCoins(d)
		if err != nil {
			return nil, err
		}
		msg.InitialDeposit = coins
		// two floors: min_initial_deposit_ratio for the proposal, and
		// min_deposit_ratio for any single deposit — the larger applies
		ratio := params.MinInitialDepositRatio
		if ratioGreater(params.MinDepositRatio, ratio) {
			ratio = params.MinDepositRatio
		}
		if floor := scaled(minDep, ratio); below(coins, floor) {
			return nil, fmt.Errorf("deposit %s is below the smallest deposit the chain accepts, %s (%s of the %s minimum)",
				d, coinsString(floor), strings.TrimRight(strings.TrimRight(ratio, "0"), "."), coinsString(minDep))
		}
		if below(coins, minDep) {
			p.Notes = append(p.Notes, fmt.Sprintf("deposit %s is below the minimum %s: the proposal waits in the deposit period until topped up (gov.deposit)", d, coinsString(minDep)))
		}
	} else {
		msg.InitialDeposit = minDep
	}
	p.Detail["deposit"] = coinsString(msg.InitialDeposit)

	votingPeriod := params.VotingPeriod.AsDuration()
	if expedited && params.ExpeditedVotingPeriod != nil {
		votingPeriod = params.ExpeditedVotingPeriod.AsDuration()
	}
	p.Detail["voting_period"] = votingPeriod.String()

	switch kind := a.String("kind", ""); kind {
	case "text":
		if msg.Metadata == "" {
			// the SDK rejects a proposal with neither messages nor metadata
			msg.Metadata = title
		}
	case "upgrade":
		name := a.String("name", "")
		if !planNameRe.MatchString(name) {
			return nil, fmt.Errorf("upgrade name %q: use the new binary's upgrade handler name (letters, digits, . _ -)", name)
		}
		cur, blockTime, err := chainClock(c)
		if err != nil {
			return nil, err
		}
		height := a.Int("height", 0)
		if n := a.Int("in_blocks", 0); n > 0 {
			height = cur + n
		}
		if height <= cur {
			return nil, fmt.Errorf("upgrade height %d is not in the future (chain is at %d) — pass height or in_blocks", height, cur)
		}
		// the proposal must pass before the chain reaches the height
		need := int64(votingPeriod/blockTime) + 1
		margin := need / 5
		if margin < 20 {
			margin = 20
		}
		if height < cur+need+margin {
			return nil, fmt.Errorf("upgrade height %d is %d blocks away, but the vote takes %s ≈ %d blocks at %s/block — the plan would expire before it passes; use height ≥ %d",
				height, height-cur, votingPeriod, need, blockTime.Round(time.Millisecond), cur+need+margin)
		}
		eta := time.Duration(height-cur) * blockTime
		p.Notes = append(p.Notes, fmt.Sprintf("halts the chain at height %d (≈ %s from now at %s/block); every validator needs the %q binary staged by then",
			height, eta.Round(time.Minute), blockTime.Round(time.Millisecond), name))
		gov, err := ModuleAddress("gov", accountPrefix(proposer))
		if err != nil {
			return nil, err
		}
		any, err := cosmosAny(&upgradev1beta1.MsgSoftwareUpgrade{Authority: gov,
			Plan: &upgradev1beta1.Plan{Name: name, Height: height, Info: a.String("info", "")}})
		if err != nil {
			return nil, err
		}
		msg.Messages = []*anypb.Any{any}
		p.Detail["upgrade"] = fmt.Sprintf("%s at %d", name, height)
	case "messages":
		anys, err := parseMessages(a.String("messages", ""))
		if err != nil {
			return nil, err
		}
		msg.Messages = anys
		var types []string
		for _, x := range anys {
			types = append(types, x.TypeUrl)
		}
		p.Detail["messages"] = strings.Join(types, ", ")
	default:
		return nil, fmt.Errorf("kind %q: want text, upgrade or messages", kind)
	}
	p.Msg = msg
	return p, nil
}

func (proposeTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	proposer, err := common.Account(c)
	if err != nil {
		return nil, err
	}
	p, err := Build(c, a, proposer)
	if err != nil {
		return nil, err
	}
	meta := p.Detail
	for i, n := range p.Notes {
		meta[fmt.Sprintf("note_%d", i+1)] = n
	}
	res, err := common.BroadcastMsgs(c, tx.Msgs{p.Msg}, a.String("memo", ""), meta, common.TxOpts(a))
	if err != nil {
		return res, err
	}
	if id := latestByProposer(c, proposer, p.Msg.Title); id > 0 {
		res.Text += fmt.Sprintf("\nproposal id: %d — vote with val.vote proposal=%d; follow with wait.until condition=proposal-status value=%d:PASSED", id, id, id)
		if res.Data == nil {
			res.Data = map[string]any{}
		}
		res.Data["proposal_id"] = id
	}
	for _, n := range p.Notes {
		res.Text += "\nnote: " + n
	}
	return res, nil
}

type depositTool struct{}

func (depositTool) Name() string { return "gov.deposit" }
func (depositTool) Desc() string {
	return "Add a deposit to a governance proposal (default: the amount still missing to reach the minimum)"
}
func (depositTool) Schema() map[string]any {
	return toolkit.ObjSchema(common.WithTx(map[string]any{
		"proposal": toolkit.Int("proposal id"),
		"amount":   toolkit.Str("deposit, e.g. 5000000stake (default: what's missing to the minimum)"),
		"memo":     toolkit.Str("tx memo"),
	}), "proposal")
}
func (depositTool) Tier() toolkit.Tier { return toolkit.TierOnChain }

func (depositTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	id := uint64(a.Int("proposal", 0))
	if id == 0 {
		return nil, fmt.Errorf("proposal id is required")
	}
	depositor, err := common.Account(c)
	if err != nil {
		return nil, err
	}
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	pr, err := g.GovV1.Proposal(c, &govv1.QueryProposalRequest{ProposalId: id})
	if err != nil {
		return nil, fmt.Errorf("proposal %d: %w", id, err)
	}
	var amount []*basev1beta1.Coin
	if s := strings.TrimSpace(a.String("amount", "")); s != "" {
		if amount, err = parseCoins(s); err != nil {
			return nil, err
		}
	} else {
		params, err := g.GovV1.Params(c, &govv1.QueryParamsRequest{ParamsType: "deposit"})
		if err != nil || params.Params == nil {
			return nil, fmt.Errorf("gov params: %v", err)
		}
		min := params.Params.MinDeposit
		if pr.Proposal.Expedited && len(params.Params.ExpeditedMinDeposit) > 0 {
			min = params.Params.ExpeditedMinDeposit
		}
		amount = missing(min, pr.Proposal.TotalDeposit)
		if len(amount) == 0 {
			return nil, fmt.Errorf("proposal %d already has the minimum deposit (%s) — pass amount to add more", id, coinsString(pr.Proposal.TotalDeposit))
		}
	}
	msg := &govv1.MsgDeposit{ProposalId: id, Depositor: depositor, Amount: amount}
	return common.BroadcastMsgs(c, tx.Msgs{msg}, a.String("memo", ""),
		map[string]string{"action": "gov-deposit", "proposal": fmt.Sprint(id), "amount": coinsString(amount)}, common.TxOpts(a))
}

// --- helpers ------------------------------------------------------------------

// chainClock returns the latest height and the average block time over
// the last 100 blocks.
func chainClock(c *toolkit.Context) (int64, time.Duration, error) {
	cc, err := c.Comet()
	if err != nil {
		return 0, 0, err
	}
	st, err := cc.Status(c)
	if err != nil {
		return 0, 0, err
	}
	h := st.SyncInfo.LatestBlockHeight
	back := int64(100)
	if h <= back {
		back = h - 1
	}
	if back < 1 {
		return h, 5 * time.Second, nil
	}
	old := h - back
	b, err := cc.Block(c, &old)
	if err != nil {
		return h, 5 * time.Second, nil
	}
	bt := st.SyncInfo.LatestBlockTime.Sub(b.Block.Time) / time.Duration(back)
	if bt <= 0 {
		bt = 5 * time.Second
	}
	return h, bt, nil
}

var coinRe = regexp.MustCompile(`^([0-9]+)([a-zA-Z][a-zA-Z0-9/:._-]{1,127})$`)

func parseCoins(s string) ([]*basev1beta1.Coin, error) {
	var out []*basev1beta1.Coin
	for _, part := range strings.Split(s, ",") {
		m := coinRe.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return nil, fmt.Errorf("coin %q: want <amount><denom>, e.g. 10000000stake", part)
		}
		out = append(out, &basev1beta1.Coin{Amount: m[1], Denom: m[2]})
	}
	return out, nil
}

func coinsString(cs []*basev1beta1.Coin) string {
	var parts []string
	for _, c := range cs {
		parts = append(parts, c.Amount+c.Denom)
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, ",")
}

func amountOf(cs []*basev1beta1.Coin, denom string) *big.Int {
	for _, c := range cs {
		if c.Denom == denom {
			v, ok := new(big.Int).SetString(c.Amount, 10)
			if ok {
				return v
			}
		}
	}
	return new(big.Int)
}

// below reports whether have is short of want in any of want's denoms.
func below(have, want []*basev1beta1.Coin) bool { return len(missing(want, have)) > 0 }

// missing is want − have, per denom, positive parts only.
func missing(want, have []*basev1beta1.Coin) []*basev1beta1.Coin {
	var out []*basev1beta1.Coin
	for _, w := range want {
		need := amountOf([]*basev1beta1.Coin{w}, w.Denom)
		need.Sub(need, amountOf(have, w.Denom))
		if need.Sign() > 0 {
			out = append(out, &basev1beta1.Coin{Denom: w.Denom, Amount: need.String()})
		}
	}
	return out
}

func ratioGreater(a, b string) bool {
	x, ok1 := new(big.Rat).SetString(strings.TrimSpace(a))
	y, ok2 := new(big.Rat).SetString(strings.TrimSpace(b))
	if !ok1 {
		return false
	}
	return !ok2 || x.Cmp(y) > 0
}

// scaled multiplies coins by a decimal ratio ("0.01"), rounding up.
func scaled(cs []*basev1beta1.Coin, ratio string) []*basev1beta1.Coin {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(ratio))
	if !ok || r.Sign() <= 0 {
		return nil
	}
	var out []*basev1beta1.Coin
	for _, c := range cs {
		v, ok := new(big.Rat).SetString(c.Amount)
		if !ok {
			continue
		}
		v.Mul(v, r)
		n := new(big.Int).Quo(v.Num(), v.Denom())
		if new(big.Rat).SetInt(n).Cmp(v) < 0 {
			n.Add(n, big.NewInt(1))
		}
		out = append(out, &basev1beta1.Coin{Denom: c.Denom, Amount: n.String()})
	}
	return out
}

func parseMessages(s string) ([]*anypb.Any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("messages is required for kind=messages")
	}
	// decode as a wrapper so protojson resolves each @type
	var wrap govv1.MsgSubmitProposal
	if err := protojson.Unmarshal([]byte(`{"messages":`+s+`}`), &wrap); err != nil {
		return nil, fmt.Errorf("messages: %w (each needs a registered \"@type\")", err)
	}
	if len(wrap.Messages) == 0 {
		return nil, fmt.Errorf("messages: empty list")
	}
	return wrap.Messages, nil
}

// latestByProposer finds the id of the proposal just submitted.
func latestByProposer(c *toolkit.Context, proposer, title string) uint64 {
	g, err := c.GRPC()
	if err != nil {
		return 0
	}
	// the initial deposit makes the proposer a depositor
	res, err := g.GovV1.Proposals(c, &govv1.QueryProposalsRequest{Depositor: proposer})
	if err != nil {
		return 0
	}
	var id uint64
	for _, p := range res.Proposals {
		if p.Title == title && p.Id > id {
			id = p.Id
		}
	}
	return id
}

// cosmosAny packs m with a Cosmos type URL ("/cosmos.x.v1.Msg…"):
// anypb.New's "type.googleapis.com/…" prefix isn't resolved by chains.
func cosmosAny(m proto.Message) (*anypb.Any, error) {
	b, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &anypb.Any{TypeUrl: "/" + string(proto.MessageName(m)), Value: b}, nil
}
