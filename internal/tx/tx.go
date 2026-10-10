// Package tx implements the transaction pipeline: build → simulate →
// (human confirm, handled by caller) → sign → broadcast. It uses only the
// generated cosmossdk.io/api protobuf types — no cosmos-sdk dependency.
package tx

import (
	// key types that appear inside messages (MsgCreateValidator's
	// consensus pubkey): registered so approvals can show them
	"context"
	_ "cosmossdk.io/api/cosmos/crypto/ed25519"
	_ "cosmossdk.io/api/cosmos/crypto/secp256k1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	abciv1beta1 "cosmossdk.io/api/cosmos/base/abci/v1beta1"
	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	signingv1beta1 "cosmossdk.io/api/cosmos/tx/signing/v1beta1"
	txv1beta1 "cosmossdk.io/api/cosmos/tx/v1beta1"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/client/grpc"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/keys"
)

// Builder assembles and signs transactions for one profile.
type Builder struct {
	conn    *grpc.Conn
	profile *config.Profile
	audit   *audit.Logger
	signer  Signer
}

// NewBuilderWith builds transactions signed by s.
func NewBuilderWith(conn *grpc.Conn, p *config.Profile, lg *audit.Logger, s Signer) *Builder {
	return &Builder{conn: conn, profile: p, audit: lg, signer: s}
}

// Signer returns the builder's signer.
func (b *Builder) Signer() Signer { return b.signer }

// NewBuilder opens the ops keyring and resolves the signer key.
func NewBuilder(ctx context.Context, conn *grpc.Conn, p *config.Profile, lg *audit.Logger) (*Builder, error) {
	if p.Signer.Key == "" {
		return nil, fmt.Errorf("profile %q has no signer.key — run `cometcli keys add` and set it", p.Name)
	}
	ring, err := keys.Open(p)
	if err != nil {
		return nil, err
	}
	k, err := ring.Get(p.Signer.Key)
	if err != nil {
		return nil, err
	}
	prefix := p.Bech32Prefix
	if prefix == "" {
		prefix = "cosmos"
	}
	addr, err := k.Bech32(prefix)
	if err != nil {
		return nil, err
	}
	return &Builder{conn: conn, profile: p, audit: lg, signer: &LocalSigner{Key: k, Addr: addr}}, nil
}

// Address returns the signer's bech32 account address.
func (b *Builder) Address() string { return b.signer.Address() }

// ValAddress returns the signer's valoper address.
func (b *Builder) ValAddress() (string, error) { return keys.ValAddress(b.signer.Address()) }

// Msgs is one message to include.
type Msgs []proto.Message

// Options controls building.
type Options struct {
	Memo        string
	GasLimit    uint64 // 0 = simulate to estimate
	GasAdjust   float64
	FeeDenom    string
	GasPrice    string // decimal, e.g. "0.025"
	AccountNum  uint64
	Sequence    uint64
	haveAccount bool
	// forcedSeq overrides the queried sequence (mempool heal); nil = use
	// the chain's. A pointer so the zero Options can't mean "sequence 0".
	forcedSeq *uint64
}

// ForceSequence overrides the queried sequence — used to heal a mismatch
// with the value the chain reported it expects.
func (o Options) ForceSequence(seq uint64) Options {
	o.forcedSeq = &seq
	return o
}

// WithAccount pins the account number/sequence explicitly (offline signing,
// or a caller that already resolved them) instead of querying the chain.
func (o Options) WithAccount(num, seq uint64) Options {
	o.AccountNum, o.Sequence, o.haveAccount = num, seq, true
	return o
}

// PinnedAccount reports whether the caller fixed num/seq explicitly — in
// which case a sequence mismatch can't be healed by rebuilding.
func (o Options) PinnedAccount() bool { return o.haveAccount }

// Built is a signed transaction plus its decoded rendering.
type Built struct {
	TxBytes []byte
	Doc     Doc // human/agent-readable rendering
}

// Doc is the decoded transaction for display.
type Doc struct {
	Signer   string   `json:"signer"`
	ChainID  string   `json:"chain_id"`
	Account  string   `json:"account"`
	AccNum   uint64   `json:"account_number"`
	Seq      uint64   `json:"sequence"`
	Msgs     []string `json:"messages"`
	Fee      string   `json:"fee"`
	GasLimit uint64   `json:"gas_limit"`
	GasNote  string   `json:"gas_note,omitempty"`
	Memo     string   `json:"memo,omitempty"`
}

// Prepared is a transaction ready to approve and sign: gas settled, the
// document rendered, nothing signed yet (for the container signer).
type Prepared struct {
	Msgs   Msgs
	Body   []byte
	AccNum uint64
	Seq    uint64
	Opt    Options
	Doc    Doc
}

// DefaultGas is used when gas can't be simulated.
const DefaultGas = 300_000

func (d Doc) String() string {
	b, _ := json.MarshalIndent(d, "", "  ")
	return string(b)
}

// Build prepares and signs in one step (no approval in between).
func (b *Builder) Build(ctx context.Context, msgs Msgs, opt Options) (*Built, error) {
	p, err := b.Prepare(ctx, msgs, opt)
	if err != nil {
		return nil, err
	}
	raw, err := b.Sign(ctx, p)
	if err != nil {
		return nil, err
	}
	return &Built{TxBytes: raw, Doc: p.Doc}, nil
}

// Prepare resolves the account, builds the body and settles gas — by
// simulation (x GasAdjust) unless GasLimit is set, or DefaultGas when the
// signer can't produce a simulation tx.
func (b *Builder) Prepare(ctx context.Context, msgs Msgs, opt Options) (*Prepared, error) {
	var num, seq uint64
	if opt.haveAccount {
		num, seq = opt.AccountNum, opt.Sequence
	} else {
		var err error
		num, seq, err = b.conn.Account(ctx, b.signer.Address())
		if err != nil {
			return nil, err
		}
	}
	if opt.forcedSeq != nil {
		seq = *opt.forcedSeq
	}
	if opt.GasAdjust == 0 {
		opt.GasAdjust = 1.4
	}
	body, err := b.body(msgs, opt.Memo)
	if err != nil {
		return nil, err
	}
	p := &Prepared{Msgs: msgs, Body: body, AccNum: num, Seq: seq, Opt: opt}
	gasNote := ""
	if opt.GasLimit == 0 {
		sim, err := b.signer.SimBytes(ctx, b, p)
		if err != nil {
			return nil, err
		}
		if sim == nil {
			p.Opt.GasLimit = DefaultGas
			gasNote = fmt.Sprintf("not simulated (the signer's public key isn't on chain yet) — using %d; pass gas-limit to override", DefaultGas)
		} else {
			used, err := b.Simulate(ctx, sim)
			if err != nil {
				return nil, fmt.Errorf("gas simulation: %w", err)
			}
			p.Opt.GasLimit = uint64(float64(used) * opt.GasAdjust)
		}
	}
	p.Doc = b.doc(p)
	p.Doc.GasNote = gasNote
	return p, nil
}

// Sign produces the final tx bytes.
func (b *Builder) Sign(ctx context.Context, p *Prepared) ([]byte, error) {
	return b.signer.Sign(ctx, b, p)
}

func (b *Builder) doc(p *Prepared) Doc {
	d := Doc{Signer: b.signer.Describe(), ChainID: b.profile.ChainID, Account: b.signer.Address(),
		AccNum: p.AccNum, Seq: p.Seq, Memo: p.Opt.Memo, GasLimit: p.Opt.GasLimit}
	for _, m := range p.Msgs {
		d.Msgs = append(d.Msgs, describeMsg(m))
	}
	if fee, err := b.fee(p.Opt); err == nil && len(fee.Amount) > 0 {
		d.Fee = fee.Amount[0].Amount + fee.Amount[0].Denom
	}
	return d
}

func (b *Builder) body(msgs Msgs, memo string) ([]byte, error) {
	tb := &txv1beta1.TxBody{Memo: memo}
	for _, m := range msgs {
		v, err := proto.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("packing msg %T: %w", m, err)
		}
		// Cosmos chains expect "/full.name" type URLs — anypb.New would
		// emit "type.googleapis.com/full.name" which evmd's decoder rejects.
		tb.Messages = append(tb.Messages, &anypb.Any{
			TypeUrl: "/" + string(m.ProtoReflect().Descriptor().FullName()),
			Value:   v,
		})
	}
	return proto.Marshal(tb)
}

func (b *Builder) fee(opt Options) (*txv1beta1.Fee, error) {
	gas := opt.GasLimit
	if gas == 0 {
		gas = 30_000_000 // simulation ceiling
	}
	denom := opt.FeeDenom
	if denom == "" {
		denom = b.profile.Metadata["fee_denom"]
	}
	if denom == "" {
		return nil, fmt.Errorf("no fee denom — set metadata.fee_denom in profile or pass --fee-denom")
	}
	price := opt.GasPrice
	if price == "" {
		price = b.profile.Metadata["gas_price"]
	}
	if price == "" {
		price = "0.025"
	}
	var pf float64
	if _, err := fmt.Sscan(price, &pf); err != nil {
		return nil, fmt.Errorf("bad gas price %q", price)
	}
	amount := uint64(float64(gas)*pf + 0.5)
	return &txv1beta1.Fee{
		Amount:   []*basev1beta1.Coin{{Denom: denom, Amount: fmt.Sprintf("%d", amount)}},
		GasLimit: gas,
	}, nil
}

func (b *Builder) authInfo(pkAny *anypb.Any, opt Options, seq uint64) ([]byte, error) {
	fee, err := b.fee(opt)
	if err != nil {
		return nil, err
	}
	ai := &txv1beta1.AuthInfo{
		SignerInfos: []*txv1beta1.SignerInfo{{
			PublicKey: pkAny,
			ModeInfo: &txv1beta1.ModeInfo{
				Sum: &txv1beta1.ModeInfo_Single_{
					Single: &txv1beta1.ModeInfo_Single{Mode: signingv1beta1.SignMode_SIGN_MODE_DIRECT},
				},
			},
			Sequence: seq,
		}},
		Fee: fee,
	}
	return proto.Marshal(ai)
}

// Simulate runs the tx against the node's simulation endpoint, returning gas used.
func (b *Builder) Simulate(ctx context.Context, txBytes []byte) (uint64, error) {
	res, err := b.conn.Tx.Simulate(ctx, &txv1beta1.SimulateRequest{TxBytes: txBytes})
	if err != nil {
		return 0, err
	}
	if b.audit != nil {
		b.audit.Tx(b.profile.Name, "simulate", map[string]any{"gas_used": res.GasInfo.GasUsed})
	}
	return res.GasInfo.GasUsed, nil
}

// Broadcast sends the tx and returns the tx hash + CheckTx code.
// SYNC mode means "accepted to mempool" — a non-zero code is a definite
// rejection; a zero code still needs Confirm() for the deliver_tx result.
func (b *Builder) Broadcast(ctx context.Context, txBytes []byte) (hash string, code uint32, rawLog string, err error) {
	res, err := b.conn.Tx.BroadcastTx(ctx, &txv1beta1.BroadcastTxRequest{
		TxBytes: txBytes,
		Mode:    txv1beta1.BroadcastMode_BROADCAST_MODE_SYNC,
	})
	if err != nil {
		return "", 0, "", err
	}
	r := res.TxResponse
	if b.audit != nil {
		b.audit.Tx(b.profile.Name, "broadcast", map[string]any{"hash": r.Txhash, "code": r.Code, "raw_log": r.RawLog,
			"tx_bytes": base64.StdEncoding.EncodeToString(txBytes)})
	}
	return r.Txhash, r.Code, r.RawLog, nil
}

// ConfirmTimeout and ConfirmPoll bound how long Confirm waits for a tx to
// be committed (variables so tests can shorten them).
var (
	ConfirmTimeout = 30 * time.Second
	ConfirmPoll    = 1500 * time.Millisecond
)

// Confirm polls GetTx until the tx is committed (or ctx/timeout expires) and
// returns the final on-chain result — the code that actually matters.
func (b *Builder) Confirm(ctx context.Context, hash string) (*abciv1beta1.TxResponse, error) {
	deadline := time.Now().Add(ConfirmTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		res, err := b.GetTx(ctx, hash)
		if err == nil && res.TxResponse != nil {
			return res.TxResponse, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ConfirmPoll):
		}
	}
	return nil, fmt.Errorf("tx %s not committed within %s (last: %v)", hash, ConfirmTimeout, lastErr)
}

// GetTx fetches a committed tx by hash.
func (b *Builder) GetTx(ctx context.Context, hash string) (*txv1beta1.GetTxResponse, error) {
	return b.conn.Tx.GetTx(ctx, &txv1beta1.GetTxRequest{Hash: hash})
}

func describeMsg(m proto.Message) string {
	name := string(m.ProtoReflect().Descriptor().FullName())
	js := protojson.MarshalOptions{Multiline: false}.Format(m)
	return "/" + name + " " + abbrev(js)
}

func abbrev(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
