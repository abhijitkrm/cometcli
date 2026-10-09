package tx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	secp256k1api "cosmossdk.io/api/cosmos/crypto/secp256k1"
	txv1beta1 "cosmossdk.io/api/cosmos/tx/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/abhijitkrm/cometcli/internal/keys"
)

// Signer produces signed transactions for one account.
type Signer interface {
	// Address is the signer's bech32 account address.
	Address() string
	// Describe names the signer for the approval document.
	Describe() string
	// SimBytes returns tx bytes for gas simulation, or nil when the signer
	// can't build one without signing interactively.
	SimBytes(ctx context.Context, b *Builder, p *Prepared) ([]byte, error)
	// Sign returns the final tx bytes.
	Sign(ctx context.Context, b *Builder, p *Prepared) ([]byte, error)
}

// LocalSigner signs with a key from cometcli's own keyring.
type LocalSigner struct {
	Key  *keys.Key
	Addr string
}

func (s *LocalSigner) Address() string  { return s.Addr }
func (s *LocalSigner) Describe() string { return "cometcli keyring (" + s.Key.Name + ")" }

// SimBytes builds an unsigned tx carrying the public key: the SDK skips
// signature checks when simulating, and the key isn't touched before the
// operator approves the signature.
func (s *LocalSigner) SimBytes(_ context.Context, b *Builder, p *Prepared) ([]byte, error) {
	pk, err := s.pubKeyAny()
	if err != nil {
		return nil, err
	}
	authInfo, err := b.authInfo(pk, p.Opt, p.Seq)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(&txv1beta1.TxRaw{BodyBytes: p.Body, AuthInfoBytes: authInfo, Signatures: [][]byte{{}}})
}

func (s *LocalSigner) Sign(_ context.Context, b *Builder, p *Prepared) ([]byte, error) {
	pk, err := s.pubKeyAny()
	if err != nil {
		return nil, err
	}
	authInfo, err := b.authInfo(pk, p.Opt, p.Seq)
	if err != nil {
		return nil, err
	}
	sd, err := proto.Marshal(&txv1beta1.SignDoc{BodyBytes: p.Body, AuthInfoBytes: authInfo, ChainId: b.profile.ChainID, AccountNumber: p.AccNum})
	if err != nil {
		return nil, err
	}
	return proto.Marshal(&txv1beta1.TxRaw{BodyBytes: p.Body, AuthInfoBytes: authInfo, Signatures: [][]byte{s.Key.Sign(sd)}})
}

// pubKeyAny emits the pubkey Any matching the key algorithm. eth_secp256k1
// uses the type URL cosmos-evm registers.
func (s *LocalSigner) pubKeyAny() (*anypb.Any, error) {
	if s.Key.Algo == keys.AlgoEthSecp256k1 {
		var v []byte
		v = protowire.AppendTag(v, 1, protowire.BytesType)
		v = protowire.AppendBytes(v, s.Key.PubKey)
		return &anypb.Any{TypeUrl: "/cosmos.evm.crypto.v1.ethsecp256k1.PubKey", Value: v}, nil
	}
	v, err := proto.Marshal(&secp256k1api.PubKey{Key: s.Key.PubKey})
	if err != nil {
		return nil, err
	}
	return &anypb.Any{TypeUrl: "/cosmos.crypto.secp256k1.PubKey", Value: v}, nil
}

// ContainerSigner signs inside the node's container with its own keyring:
// `docker exec <container> evmd tx sign … && evmd tx encode …`. The key
// never leaves the node; a file-keyring password is asked for at signing
// time and passed on stdin only.
type ContainerSigner struct {
	Container string // docker container name
	Key       string // key name in the container's keyring
	Keyring   string // test | file
	Home      string // evmd --home ("" = default)
	Binary    string // chain binary in the container (default evmd)
	Addr      string // bech32 account address of Key
	// PubKey is the account's on-chain public key, used to build a
	// simulation tx; nil means gas can't be simulated.
	PubKey *anypb.Any
	// Run executes a shell script on the node's host with stdin.
	Run func(ctx context.Context, script string, stdin []byte) (output string, err error)
	// Secret asks the operator for the keyring password.
	Secret func(prompt string) (string, error)
}

func (s *ContainerSigner) Address() string { return s.Addr }
func (s *ContainerSigner) Describe() string {
	return fmt.Sprintf("container %s (key %s, %s keyring)", s.Container, s.Key, s.Keyring)
}

// SimBytes builds an unsigned tx carrying the on-chain public key; the
// SDK skips signature checks when simulating.
func (s *ContainerSigner) SimBytes(_ context.Context, b *Builder, p *Prepared) ([]byte, error) {
	if s.PubKey == nil {
		return nil, nil
	}
	authInfo, err := b.authInfo(s.PubKey, p.Opt, p.Seq)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(&txv1beta1.TxRaw{BodyBytes: p.Body, AuthInfoBytes: authInfo, Signatures: [][]byte{{}}})
}

const txMarker = "__COMETCLI_TX__"

func (s *ContainerSigner) Sign(ctx context.Context, b *Builder, p *Prepared) ([]byte, error) {
	unsigned, err := b.unsignedJSON(p)
	if err != nil {
		return nil, err
	}
	var stdin []byte
	if s.Keyring == "file" {
		if s.Secret == nil {
			return nil, fmt.Errorf("the %s keyring in %s needs a password and there's no one to ask — run interactively or set COMETCLI_CONTAINER_KEYRING_PASSWORD", s.Keyring, s.Container)
		}
		pw, err := s.Secret(fmt.Sprintf("Keyring password for %s in %s", s.Key, s.Container))
		if err != nil {
			return nil, err
		}
		stdin = []byte(pw + "\n")
	}
	bin := s.Binary
	if bin == "" {
		bin = "evmd"
	}
	home := ""
	if s.Home != "" {
		home = " --home " + q(s.Home)
	}
	inner := fmt.Sprintf(`set -e
umask 077; u=/tmp/cometcli-$$-unsigned.json; s=/tmp/cometcli-$$-signed.json; trap 'rm -f "$u" "$s"' EXIT
printf %%s %s | base64 -d > "$u"
%s%s tx sign "$u" --from %s --keyring-backend %s --chain-id %s --offline --account-number %d --sequence %d --sign-mode direct --output-document "$s"
printf '%s%%s\n' "$(%s%s tx encode "$s")"`,
		q(base64.StdEncoding.EncodeToString(unsigned)), q(bin), home, q(s.Key), q(s.Keyring), q(b.profile.ChainID),
		p.AccNum, p.Seq, txMarker, q(bin), home)
	out, err := s.Run(ctx, "docker exec -i "+q(s.Container)+" sh -c "+q(inner), stdin)
	if i := strings.LastIndex(out, txMarker); i >= 0 {
		enc := strings.TrimSpace(strings.SplitN(out[i+len(txMarker):], "\n", 2)[0])
		raw, derr := base64.StdEncoding.DecodeString(enc)
		if derr == nil && len(raw) > 0 {
			return raw, nil
		}
	}
	return nil, containerSignError(s, out, err)
}

// containerSignError explains a failed container signing.
func containerSignError(s *ContainerSigner, out string, err error) error {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "incorrect passphrase") || strings.Contains(low, "ciphertext decryption failed") || strings.Contains(low, "aes.keyunwrap") || strings.Contains(low, "invalid password"):
		return fmt.Errorf("wrong keyring password for %s in %s", s.Key, s.Container)
	case strings.Contains(low, "key not found") || strings.Contains(low, "not a valid name or address"):
		return fmt.Errorf("key %q not found in %s's %s keyring (set signer.container_key / container_keyring)", s.Key, s.Container, s.Keyring)
	case strings.Contains(low, "no such container") || strings.Contains(low, "is not running"):
		return fmt.Errorf("container %s is not running", s.Container)
	}
	msg := strings.TrimSpace(out)
	// a CLI failure prints "Error: …" then its whole --help: the error
	// line is the news, not the tail of the flag list
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Error:") {
			msg = l
			break
		}
	}
	if len(msg) > 400 {
		msg = msg[len(msg)-400:]
	}
	if err != nil {
		return fmt.Errorf("signing in %s failed: %v: %s", s.Container, err, msg)
	}
	return fmt.Errorf("signing in %s produced no transaction: %s", s.Container, msg)
}

// unsignedJSON renders the tx the way `--generate-only` does, for `tx sign`.
func (b *Builder) unsignedJSON(p *Prepared) ([]byte, error) {
	var msgs []json.RawMessage
	for _, m := range p.Msgs {
		js, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}.Marshal(m)
		if err != nil {
			return nil, err
		}
		var obj map[string]any
		if err := json.Unmarshal(js, &obj); err != nil {
			return nil, err
		}
		obj["@type"] = "/" + string(m.ProtoReflect().Descriptor().FullName())
		raw, _ := json.Marshal(obj)
		msgs = append(msgs, raw)
	}
	fee, err := b.fee(p.Opt)
	if err != nil {
		return nil, err
	}
	var amount []map[string]string
	for _, c := range fee.Amount {
		amount = append(amount, map[string]string{"denom": c.Denom, "amount": c.Amount})
	}
	return json.Marshal(map[string]any{
		"body": map[string]any{
			"messages": msgs, "memo": p.Opt.Memo, "timeout_height": "0",
			"extension_options": []any{}, "non_critical_extension_options": []any{},
		},
		"auth_info": map[string]any{
			"signer_infos": []any{},
			"fee":          map[string]any{"amount": amount, "gas_limit": fmt.Sprint(fee.GasLimit), "payer": "", "granter": ""},
		},
		"signatures": []any{},
	})
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
