// Package fakenode is an in-process stand-in for a cosmos-evm node's gRPC
// API, for tests of the transaction pipeline and node tools. It is
// strict where it matters: every broadcast transaction is decoded, its
// secp256k1 signature verified over the SIGN_MODE_DIRECT sign doc
// (keccak256 for eth_secp256k1 keys, sha256 for cosmos secp256k1), and
// its sequence checked, returning the SDK's own error text on mismatch.
package fakenode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	authv1beta1 "cosmossdk.io/api/cosmos/auth/v1beta1"
	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	abciv1beta1 "cosmossdk.io/api/cosmos/base/abci/v1beta1"
	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	secp256k1api "cosmossdk.io/api/cosmos/crypto/secp256k1"
	distv1beta1 "cosmossdk.io/api/cosmos/distribution/v1beta1"
	govv1 "cosmossdk.io/api/cosmos/gov/v1"
	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	txv1beta1 "cosmossdk.io/api/cosmos/tx/v1beta1"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/keys"
)

// Tx is a transaction the node accepted to its mempool.
type Tx struct {
	Hash string
	Seq  uint64
	Msgs []*anypb.Any
	Memo string
	Fee  *txv1beta1.Fee
}

// Node is the fake. Configure its fields before or between calls; all
// access is mutex-guarded.
type Node struct {
	Addr    string // host:port to dial
	ChainID string

	mu sync.Mutex
	// account state
	AccNum uint64
	Seq    uint64 // the chain's next expected sequence
	// PubKey, when set, is returned as the account's on-chain public key.
	PubKey *anypb.Any
	// StaleSeq, when set, is what account queries report (a tx pending in
	// the mempool makes the query lag the CheckTx state).
	StaleSeq *uint64
	// SimGas is what Simulate reports.
	SimGas uint64
	// CheckCode/CheckLog reject at CheckTx; DeliverCode/DeliverLog fail
	// in the block. NeverCommit makes GetTx return NotFound forever.
	CheckCode   uint32
	CheckLog    string
	DeliverCode uint32
	DeliverLog  string
	NeverCommit bool
	// CheckFailOnce rejects the next broadcast at CheckTx with this code
	// and log, then clears itself.
	CheckFailOnce *struct {
		Code uint32
		Log  string
	}
	// OnCommit runs (under the node's lock) for each accepted tx — tests
	// use it to apply a tx's effect to the simulated chain.
	OnCommit func(n *Node, tx Tx)
	// Validator, SigningInfo and Proposals back the staking/slashing/gov
	// queries tools make.
	Validator   *stakingv1beta1.Validator
	SigningInfo *slashingv1beta1.ValidatorSigningInfo
	Window      int64
	// DowntimeJail is slashing's downtime_jail_duration.
	DowntimeJail time.Duration
	// SelfDelegation is the operator's own delegation (tokens).
	SelfDelegation string
	// Balance is the signer account's fee-denom balance.
	Balance   string
	Proposals []*govv1.Proposal

	Simulations int
	simSigs     [][]byte // signatures of the latest simulated tx
	Broadcasts  []Tx     // accepted to the mempool, in order
	Rejected    []string // raw logs of rejected broadcasts
	committed   map[string]*abciv1beta1.TxResponse
	height      int64
	srv         *grpc.Server
}

// Start runs a fake node for the test's lifetime.
func Start(t testing.TB, chainID string) *Node {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{Addr: ln.Addr().String(), ChainID: chainID, AccNum: 7, SimGas: 100_000, Window: 10_000,
		DowntimeJail: 10 * time.Minute, SelfDelegation: "1000000000000000000", Balance: "1000000000000000000",
		committed: map[string]*abciv1beta1.TxResponse{}, height: 1000}
	n.srv = grpc.NewServer()
	authv1beta1.RegisterQueryServer(n.srv, &authSrv{n: n})
	txv1beta1.RegisterServiceServer(n.srv, &txSrv{n: n})
	stakingv1beta1.RegisterQueryServer(n.srv, &stakingSrv{n: n})
	slashingv1beta1.RegisterQueryServer(n.srv, &slashingSrv{n: n})
	distv1beta1.RegisterQueryServer(n.srv, &distSrv{})
	bankv1beta1.RegisterQueryServer(n.srv, &bankSrv{n: n})
	govv1.RegisterQueryServer(n.srv, &govSrv{n: n})
	go func() { _ = n.srv.Serve(ln) }()
	t.Cleanup(n.srv.Stop)
	return n
}

// Set mutates the node under its lock.
func (n *Node) Set(fn func(n *Node)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fn(n)
}

// Snapshot returns copies of what the node saw.
func (n *Node) Snapshot() (broadcasts []Tx, rejected []string, sims int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Tx(nil), n.Broadcasts...), append([]string(nil), n.Rejected...), n.Simulations
}

// --- auth ------------------------------------------------------------------

type authSrv struct {
	authv1beta1.UnimplementedQueryServer
	n *Node
}

func (s *authSrv) Account(_ context.Context, r *authv1beta1.QueryAccountRequest) (*authv1beta1.QueryAccountResponse, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	seq := s.n.Seq
	if s.n.StaleSeq != nil {
		seq = *s.n.StaleSeq
	}
	acc := &authv1beta1.BaseAccount{Address: r.Address, AccountNumber: s.n.AccNum, Sequence: seq}
	if s.n.PubKey != nil {
		acc.PubKey = &anypb.Any{TypeUrl: s.n.PubKey.TypeUrl, Value: s.n.PubKey.Value}
	}
	b, _ := proto.Marshal(acc)
	return &authv1beta1.QueryAccountResponse{Account: &anypb.Any{TypeUrl: "/cosmos.auth.v1beta1.BaseAccount", Value: b}}, nil
}

// --- tx --------------------------------------------------------------------

type txSrv struct {
	txv1beta1.UnimplementedServiceServer
	n *Node
}

// decoded is a parsed and verified transaction.
type decoded struct {
	body *txv1beta1.TxBody
	auth *txv1beta1.AuthInfo
	seq  uint64
}

// verify decodes raw tx bytes and checks the signature against the sign
// doc the chain would build. It does not check the sequence.
func (n *Node) verify(raw []byte, accNum uint64) (*decoded, error) {
	return n.verifyTx(raw, accNum, false)
}

// verifyTx checks a tx; when simulating, an empty signature is accepted
// (the SDK skips signature checks in simulation).
func (n *Node) verifyTx(raw []byte, accNum uint64, simulate bool) (*decoded, error) {
	var tr txv1beta1.TxRaw
	if err := proto.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("tx parse error: %w", err)
	}
	var body txv1beta1.TxBody
	var ai txv1beta1.AuthInfo
	if err := proto.Unmarshal(tr.BodyBytes, &body); err != nil {
		return nil, fmt.Errorf("body parse error: %w", err)
	}
	if err := proto.Unmarshal(tr.AuthInfoBytes, &ai); err != nil {
		return nil, fmt.Errorf("auth info parse error: %w", err)
	}
	for _, m := range body.Messages {
		if !strings.HasPrefix(m.TypeUrl, "/") || strings.Contains(m.TypeUrl, "googleapis") {
			return nil, fmt.Errorf("unable to resolve type URL %s", m.TypeUrl)
		}
	}
	if len(ai.SignerInfos) != 1 || len(tr.Signatures) != 1 {
		return nil, fmt.Errorf("expected one signer and one signature")
	}
	if simulate && len(tr.Signatures[0]) == 0 {
		return &decoded{body: &body, auth: &ai, seq: ai.SignerInfos[0].Sequence}, nil
	}
	sig := tr.Signatures[0]
	if len(sig) == 65 && keccakKey(ai.SignerInfos[0]) {
		sig = sig[:64] // cosmos-evm ethsecp256k1 accepts r||s||v and drops v
	}
	if len(sig) != 64 {
		return nil, fmt.Errorf("expected a 64-byte signature (65 with a recovery id for eth_secp256k1)")
	}
	si := ai.SignerInfos[0]
	if si.ModeInfo.GetSingle().GetMode().String() != "SIGN_MODE_DIRECT" {
		return nil, fmt.Errorf("unsupported sign mode")
	}
	var pubBytes []byte
	keccak := false
	switch si.PublicKey.GetTypeUrl() {
	case "/cosmos.crypto.secp256k1.PubKey":
		var pk secp256k1api.PubKey
		if err := proto.Unmarshal(si.PublicKey.Value, &pk); err != nil {
			return nil, err
		}
		pubBytes = pk.Key
	case "/cosmos.evm.crypto.v1.ethsecp256k1.PubKey":
		pubBytes = fieldBytes(si.PublicKey.Value, 1)
		keccak = true
	default:
		return nil, fmt.Errorf("unknown pubkey type %q", si.PublicKey.GetTypeUrl())
	}
	pub, err := secp256k1.ParsePubKey(pubBytes)
	if err != nil {
		return nil, fmt.Errorf("bad pubkey: %w", err)
	}
	sd, _ := proto.Marshal(&txv1beta1.SignDoc{BodyBytes: tr.BodyBytes, AuthInfoBytes: tr.AuthInfoBytes, ChainId: n.ChainID, AccountNumber: accNum})
	var h []byte
	if keccak {
		k := sha3.NewLegacyKeccak256()
		k.Write(sd)
		h = k.Sum(nil)
	} else {
		s := sha256.Sum256(sd)
		h = s[:]
	}
	var r, s secp256k1.ModNScalar
	r.SetByteSlice(sig[:32])
	s.SetByteSlice(sig[32:64])
	if !ecdsa.NewSignature(&r, &s).Verify(h, pub) {
		return nil, fmt.Errorf("signature verification failed; please verify account number (%d) and chain-id (%s): unauthorized", accNum, n.ChainID)
	}
	return &decoded{body: &body, auth: &ai, seq: si.Sequence}, nil
}

func (s *txSrv) Simulate(_ context.Context, r *txv1beta1.SimulateRequest) (*txv1beta1.SimulateResponse, error) {
	n := s.n
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Simulations++
	var tr txv1beta1.TxRaw
	if proto.Unmarshal(r.TxBytes, &tr) == nil {
		n.simSigs = tr.Signatures
	}
	d, err := n.verifyTx(r.TxBytes, n.AccNum, true)
	if err != nil {
		return nil, status.Error(codes.Unknown, err.Error())
	}
	if d.seq != n.Seq {
		return nil, status.Error(codes.Unknown, fmt.Sprintf("account sequence mismatch, expected %d, got %d: incorrect account sequence", n.Seq, d.seq))
	}
	return &txv1beta1.SimulateResponse{GasInfo: &abciv1beta1.GasInfo{GasUsed: n.SimGas, GasWanted: n.SimGas}}, nil
}

func (s *txSrv) BroadcastTx(_ context.Context, r *txv1beta1.BroadcastTxRequest) (*txv1beta1.BroadcastTxResponse, error) {
	n := s.n
	n.mu.Lock()
	defer n.mu.Unlock()
	sum := sha256.Sum256(r.TxBytes)
	hash := strings.ToUpper(hex.EncodeToString(sum[:]))
	reject := func(code uint32, log string) (*txv1beta1.BroadcastTxResponse, error) {
		n.Rejected = append(n.Rejected, log)
		return &txv1beta1.BroadcastTxResponse{TxResponse: &abciv1beta1.TxResponse{Txhash: hash, Code: code, RawLog: log}}, nil
	}
	d, err := n.verify(r.TxBytes, n.AccNum)
	if err != nil {
		return reject(4, err.Error())
	}
	if d.seq != n.Seq {
		return reject(32, fmt.Sprintf("account sequence mismatch, expected %d, got %d: incorrect account sequence", n.Seq, d.seq))
	}
	if n.CheckCode != 0 {
		return reject(n.CheckCode, n.CheckLog)
	}
	if f := n.CheckFailOnce; f != nil {
		n.CheckFailOnce = nil
		return reject(f.Code, f.Log)
	}
	n.Seq++
	n.StaleSeq = nil
	tx := Tx{Hash: hash, Seq: d.seq, Msgs: d.body.Messages, Memo: d.body.Memo, Fee: d.auth.Fee}
	n.Broadcasts = append(n.Broadcasts, tx)
	if n.OnCommit != nil && n.DeliverCode == 0 {
		n.OnCommit(n, tx)
	}
	n.height++
	n.committed[hash] = &abciv1beta1.TxResponse{Txhash: hash, Height: n.height, Code: n.DeliverCode, RawLog: n.DeliverLog, GasUsed: int64(n.SimGas)}
	return &txv1beta1.BroadcastTxResponse{TxResponse: &abciv1beta1.TxResponse{Txhash: hash}}, nil
}

func (s *txSrv) GetTx(_ context.Context, r *txv1beta1.GetTxRequest) (*txv1beta1.GetTxResponse, error) {
	n := s.n
	n.mu.Lock()
	defer n.mu.Unlock()
	res, ok := n.committed[r.Hash]
	if !ok || n.NeverCommit {
		return nil, status.Error(codes.NotFound, "tx not found: "+r.Hash)
	}
	return &txv1beta1.GetTxResponse{TxResponse: res}, nil
}

// --- staking / slashing / distribution / gov ---------------------------------

type stakingSrv struct {
	stakingv1beta1.UnimplementedQueryServer
	n *Node
}

// Validators lists the node's validator plus another one, so lookups by
// consensus key must actually match.
func (s *stakingSrv) Validators(_ context.Context, _ *stakingv1beta1.QueryValidatorsRequest) (*stakingv1beta1.QueryValidatorsResponse, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	other := &stakingv1beta1.Validator{OperatorAddress: "cosmosvaloper1other", ConsensusPubkey: &anypb.Any{TypeUrl: "/cosmos.crypto.ed25519.PubKey", Value: []byte{0x0a, 0x20, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}}}
	vals := []*stakingv1beta1.Validator{other}
	if s.n.Validator != nil {
		vals = append(vals, s.n.Validator)
	}
	return &stakingv1beta1.QueryValidatorsResponse{Validators: vals}, nil
}

func (s *stakingSrv) Validator(_ context.Context, r *stakingv1beta1.QueryValidatorRequest) (*stakingv1beta1.QueryValidatorResponse, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.n.Validator == nil || s.n.Validator.OperatorAddress != r.ValidatorAddr {
		return nil, status.Error(codes.NotFound, "validator "+r.ValidatorAddr+" does not exist")
	}
	return &stakingv1beta1.QueryValidatorResponse{Validator: s.n.Validator}, nil
}

type slashingSrv struct {
	slashingv1beta1.UnimplementedQueryServer
	n *Node
}

func (s *slashingSrv) SigningInfo(_ context.Context, r *slashingv1beta1.QuerySigningInfoRequest) (*slashingv1beta1.QuerySigningInfoResponse, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.n.SigningInfo == nil {
		return nil, status.Error(codes.NotFound, "no signing info for "+r.ConsAddress)
	}
	return &slashingv1beta1.QuerySigningInfoResponse{ValSigningInfo: s.n.SigningInfo}, nil
}

func (s *slashingSrv) Params(context.Context, *slashingv1beta1.QueryParamsRequest) (*slashingv1beta1.QueryParamsResponse, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	return &slashingv1beta1.QueryParamsResponse{Params: &slashingv1beta1.Params{SignedBlocksWindow: s.n.Window, MinSignedPerWindow: []byte("500000000000000000"),
		DowntimeJailDuration: durationpb.New(s.n.DowntimeJail)}}, nil
}

func (s *stakingSrv) Delegation(_ context.Context, r *stakingv1beta1.QueryDelegationRequest) (*stakingv1beta1.QueryDelegationResponse, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	return &stakingv1beta1.QueryDelegationResponse{DelegationResponse: &stakingv1beta1.DelegationResponse{
		Delegation: &stakingv1beta1.Delegation{DelegatorAddress: r.DelegatorAddr, ValidatorAddress: r.ValidatorAddr},
		Balance:    &basev1beta1.Coin{Denom: "adex", Amount: s.n.SelfDelegation},
	}}, nil
}

type bankSrv struct {
	bankv1beta1.UnimplementedQueryServer
	n *Node
}

func (s *bankSrv) Balance(_ context.Context, r *bankv1beta1.QueryBalanceRequest) (*bankv1beta1.QueryBalanceResponse, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	return &bankv1beta1.QueryBalanceResponse{Balance: &basev1beta1.Coin{Denom: r.Denom, Amount: s.n.Balance}}, nil
}

// WithComet points a profile at a fake CometBFT RPC.
func WithComet(p *config.Profile, c *Comet) { p.Endpoints.Comet = c.URL }

type distSrv struct {
	distv1beta1.UnimplementedQueryServer
}

type govSrv struct {
	govv1.UnimplementedQueryServer
	n *Node
}

func (s *govSrv) Proposals(context.Context, *govv1.QueryProposalsRequest) (*govv1.QueryProposalsResponse, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	return &govv1.QueryProposalsResponse{Proposals: s.n.Proposals}, nil
}

func (s *govSrv) Vote(_ context.Context, r *govv1.QueryVoteRequest) (*govv1.QueryVoteResponse, error) {
	return nil, status.Error(codes.NotFound, fmt.Sprintf("voter %s has not voted on proposal %d", r.Voter, r.ProposalId))
}

func keccakKey(s *txv1beta1.SignerInfo) bool {
	return s.PublicKey.GetTypeUrl() == "/cosmos.evm.crypto.v1.ethsecp256k1.PubKey"
}

func fieldBytes(b []byte, num protowire.Number) []byte {
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return nil
		}
		b = b[l:]
		if typ == protowire.BytesType {
			v, l := protowire.ConsumeBytes(b)
			if l < 0 {
				return nil
			}
			if n == num {
				return v
			}
			b = b[l:]
			continue
		}
		l = protowire.ConsumeFieldValue(n, typ, b)
		if l < 0 {
			return nil
		}
		b = b[l:]
	}
	return nil
}

// Profile builds a profile pointed at the node with an ops key in a file
// keyring under a temp COMETCLI_HOME. algo is "eth_secp256k1" or
// "secp256k1".
func (n *Node) Profile(t testing.TB, algo string) *config.Profile {
	t.Helper()
	t.Setenv("COMETCLI_HOME", t.TempDir())
	t.Setenv("COMETCLI_KEYRING_PASSWORD", "test-password")
	p := &config.Profile{
		Name: "fake", ChainID: n.ChainID, Bech32Prefix: "cosmos", Role: "validator",
		Endpoints: config.Endpoints{GRPC: n.Addr},
		Signer:    config.Signer{Backend: "file", Key: "ops"},
		Metadata:  map[string]string{"fee_denom": "adex", "gas_price": "1000000000"},
	}
	ring, err := keys.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	// a fixed test key — never use outside tests
	if _, err := ring.ImportHex("ops", TestKeyHex, keys.Algo(algo)); err != nil {
		t.Fatal(err)
	}
	return p
}

// LastSimSignatures is how many signatures the latest simulated tx had.
func (n *Node) LastSimSignatures() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.simSigs)
}

// LastSimSigned reports whether the latest simulated tx was really signed.
func (n *Node) LastSimSigned() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, s := range n.simSigs {
		if len(s) > 0 {
			return true
		}
	}
	return false
}
