// Package tx implements the transaction pipeline: build → simulate →
// (human confirm, handled by caller) → sign → broadcast. It uses only the
// generated cosmossdk.io/api protobuf types — no cosmos-sdk dependency.
package tx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	secp256k1api "cosmossdk.io/api/cosmos/crypto/secp256k1"
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
	key     *keys.Key
	address string // bech32 account address of the signer
}

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
	return &Builder{conn: conn, profile: p, audit: lg, key: k, address: addr}, nil
}

// Address returns the signer's bech32 account address.
func (b *Builder) Address() string { return b.address }

// ValAddress returns the signer's valoper address.
func (b *Builder) ValAddress() (string, error) { return keys.ValAddress(b.address) }

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
}

// Built is a signed transaction plus its decoded rendering.
type Built struct {
	TxBytes []byte
	Doc     Doc // human/agent-readable rendering
}

// Doc is the decoded transaction for display.
type Doc struct {
	ChainID   string   `json:"chain_id"`
	Account   string   `json:"account"`
	AccNum    uint64   `json:"account_number"`
	Seq       uint64   `json:"sequence"`
	Msgs      []string `json:"messages"`
	Fee       string   `json:"fee"`
	GasLimit  uint64   `json:"gas_limit"`
	Memo      string   `json:"memo,omitempty"`
}

func (d Doc) String() string {
	b, _ := json.MarshalIndent(d, "", "  ")
	return string(b)
}

// Build produces a signed tx. If GasLimit==0 it first simulates to estimate
// gas (x1.4 adjust) — the signature is real either way.
func (b *Builder) Build(ctx context.Context, msgs Msgs, opt Options) (*Built, error) {
	num, seq, err := b.conn.Account(ctx, b.address)
	if err != nil {
		return nil, err
	}
	if opt.haveAccount {
		num, seq = opt.AccountNum, opt.Sequence
	}
	if opt.GasAdjust == 0 {
		opt.GasAdjust = 1.4
	}

	body, err := b.body(msgs, opt.Memo)
	if err != nil {
		return nil, err
	}
	authInfo, err := b.authInfo(opt, seq)
	if err != nil {
		return nil, err
	}
	raw, doc, err := b.sign(msgs, body, authInfo, num, seq, opt)
	if err != nil {
		return nil, err
	}

	if opt.GasLimit == 0 {
		sim, err := b.Simulate(ctx, raw)
		if err != nil {
			return nil, fmt.Errorf("gas simulation: %w", err)
		}
		opt.GasLimit = uint64(float64(sim) * opt.GasAdjust)
		authInfo, err = b.authInfo(opt, seq)
		if err != nil {
			return nil, err
		}
		raw, doc, err = b.sign(msgs, body, authInfo, num, seq, opt)
		if err != nil {
			return nil, err
		}
	}
	return &Built{TxBytes: raw, Doc: doc}, nil
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

func (b *Builder) authInfo(opt Options, seq uint64) ([]byte, error) {
	fee, err := b.fee(opt)
	if err != nil {
		return nil, err
	}
	pkAny, err := b.pubKeyAny()
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

// pubKeyAny emits the pubkey Any matching the key algorithm. eth_secp256k1
// uses the ethermint type URL kept by cosmos-evm for compatibility.
func (b *Builder) pubKeyAny() (*anypb.Any, error) {
	switch b.key.Algo {
	case keys.AlgoEthSecp256k1:
		var v []byte
		v = protowire.AppendTag(v, 1, protowire.BytesType)
		v = protowire.AppendBytes(v, b.key.PubKey)
		return &anypb.Any{TypeUrl: "/cosmos.evm.crypto.v1.ethsecp256k1.PubKey", Value: v}, nil
	default:
		v, err := proto.Marshal(&secp256k1api.PubKey{Key: b.key.PubKey})
		if err != nil {
			return nil, err
		}
		return &anypb.Any{TypeUrl: "/cosmos.crypto.secp256k1.PubKey", Value: v}, nil
	}
}

func (b *Builder) sign(msgs Msgs, body, authInfo []byte, num, seq uint64, opt Options) ([]byte, Doc, error) {
	sd := &txv1beta1.SignDoc{
		BodyBytes:     body,
		AuthInfoBytes: authInfo,
		ChainId:       b.profile.ChainID,
		AccountNumber: num,
	}
	sdBytes, err := proto.Marshal(sd)
	if err != nil {
		return nil, Doc{}, err
	}
	sig := b.key.Sign(sdBytes)
	raw := &txv1beta1.TxRaw{BodyBytes: body, AuthInfoBytes: authInfo, Signatures: [][]byte{sig}}
	rawBytes, err := proto.Marshal(raw)
	if err != nil {
		return nil, Doc{}, err
	}
	doc := Doc{ChainID: b.profile.ChainID, Account: b.address, AccNum: num, Seq: seq, Memo: opt.Memo, GasLimit: opt.GasLimit}
	for _, m := range msgs {
		doc.Msgs = append(doc.Msgs, describeMsg(m))
	}
	if fee, err := b.fee(opt); err == nil && len(fee.Amount) > 0 {
		doc.Fee = fee.Amount[0].Amount + fee.Amount[0].Denom
	}
	return rawBytes, doc, nil
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

// Broadcast sends the tx and returns the tx hash + code.
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
		b.audit.Tx(b.profile.Name, "broadcast", map[string]any{"hash": r.Txhash, "code": r.Code, "raw_log": r.RawLog})
	}
	return r.Txhash, r.Code, r.RawLog, nil
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
