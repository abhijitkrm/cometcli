package fakenode

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/anypb"

	basev1beta1 "cosmossdk.io/api/cosmos/base/v1beta1"
	signingv1beta1 "cosmossdk.io/api/cosmos/tx/signing/v1beta1"
	txv1beta1 "cosmossdk.io/api/cosmos/tx/v1beta1"

	"github.com/abhijitkrm/cometcli/internal/keys"
)

// TestKeyHex is the fixed test key every fake uses — never use it for real.
const TestKeyHex = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

// TestAddress is the eth_secp256k1 cosmos address of TestKeyHex.
func TestAddress() string {
	priv, _ := hex.DecodeString(TestKeyHex)
	pk := secp256k1.PrivKeyFromBytes(priv)
	addr := keys.AddressFor(keys.AlgoEthSecp256k1, pk.PubKey())
	k := keys.Key{Address: addr}
	s, _ := k.Bech32("cosmos")
	return s
}

// InstallFakeDocker puts a fake `docker` first on PATH: the test binary
// itself, which must call MaybeRunFakeDocker from TestMain. keyring is
// the backend the container's key lives in ("test" or "file"); password
// is the file keyring's password.
func InstallFakeDocker(t testing.TB, keyring, password string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(dir, "docker")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKENODE_DOCKER", "1")
	t.Setenv("FAKENODE_KEYRING", keyring)
	t.Setenv("FAKENODE_PASSWORD", password)
}

// MaybeRunFakeDocker runs the fake when the process was started as the
// fake docker, then exits. Call it first thing in TestMain.
func MaybeRunFakeDocker() {
	if os.Getenv("FAKENODE_DOCKER") != "1" || filepath.Base(os.Args[0]) != "docker" {
		return
	}
	os.Exit(fakeDocker(os.Args[1:]))
}

var (
	reKeyring = regexp.MustCompile(`--keyring-backend '?([a-z]+)'?`)
	reB64     = regexp.MustCompile(`printf %s '([A-Za-z0-9+/=]+)'`)
	reAccNum  = regexp.MustCompile(`--account-number (\d+)`)
	reSeq     = regexp.MustCompile(`--sequence (\d+)`)
	reChain   = regexp.MustCompile(`--chain-id '([^']+)'`)
)

// fakeDocker emulates `docker exec -i <c> sh -c <script>` running evmd.
func fakeDocker(args []string) int {
	if len(args) < 5 || args[0] != "exec" {
		fmt.Fprintln(os.Stderr, "fake docker: unsupported", args)
		return 2
	}
	script := strings.Join(args, " ") // `sh -c <script>` or a direct command
	backend := ""
	if m := reKeyring.FindStringSubmatch(script); m != nil {
		backend = m[1]
	}
	if backend != os.Getenv("FAKENODE_KEYRING") {
		fmt.Fprintln(os.Stderr, "Error: ops: key not found")
		return 1
	}
	if backend == "file" {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != os.Getenv("FAKENODE_PASSWORD") {
			fmt.Fprintln(os.Stderr, "Error: aes.KeyUnwrap(): integrity check failed. incorrect passphrase")
			return 1
		}
	}
	switch {
	case strings.Contains(script, " keys show "):
		fmt.Println(TestAddress())
		return 0
	case strings.Contains(script, " tx sign "):
		raw, err := fakeSign(script)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "(signed)")
		fmt.Println("__COMETCLI_TX__" + base64.StdEncoding.EncodeToString(raw))
		return 0
	}
	fmt.Fprintln(os.Stderr, "fake docker: unsupported command:", script)
	return 2
}

// fakeSign does what `evmd tx sign --sign-mode direct` + `tx encode` do.
func fakeSign(script string) ([]byte, error) {
	m := reB64.FindStringSubmatch(script)
	if m == nil {
		return nil, fmt.Errorf("no unsigned tx in script")
	}
	js, _ := base64.StdEncoding.DecodeString(m[1])
	var doc struct {
		Body struct {
			Messages []json.RawMessage `json:"messages"`
			Memo     string            `json:"memo"`
		} `json:"body"`
		AuthInfo struct {
			SignerInfos []any `json:"signer_infos"`
			Fee         struct {
				Amount []struct{ Denom, Amount string } `json:"amount"`
				Gas    string                           `json:"gas_limit"`
			} `json:"fee"`
		} `json:"auth_info"`
		Signatures []any `json:"signatures"`
	}
	if err := json.Unmarshal(js, &doc); err != nil {
		return nil, err
	}
	if len(doc.AuthInfo.SignerInfos) != 0 || len(doc.Signatures) != 0 {
		return nil, fmt.Errorf("unsigned tx already carries signer infos/signatures")
	}
	body := &txv1beta1.TxBody{Memo: doc.Body.Memo}
	for _, raw := range doc.Body.Messages {
		var obj map[string]json.RawMessage
		var typ string
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(obj["@type"], &typ); err != nil {
			return nil, fmt.Errorf("message without @type")
		}
		delete(obj, "@type")
		mt, err := protoregistry.GlobalTypes.FindMessageByURL(typ)
		if err != nil {
			return nil, fmt.Errorf("unable to resolve type URL %s", typ)
		}
		msg := mt.New().Interface()
		rest, _ := json.Marshal(obj)
		if err := protojson.Unmarshal(rest, msg); err != nil {
			return nil, err
		}
		v, _ := proto.Marshal(msg)
		body.Messages = append(body.Messages, &anypb.Any{TypeUrl: typ, Value: v})
	}
	gas, _ := strconv.ParseUint(doc.AuthInfo.Fee.Gas, 10, 64)
	fee := &txv1beta1.Fee{GasLimit: gas}
	for _, c := range doc.AuthInfo.Fee.Amount {
		fee.Amount = append(fee.Amount, &basev1beta1.Coin{Denom: c.Denom, Amount: c.Amount})
	}
	num, _ := strconv.ParseUint(reAccNum.FindStringSubmatch(script)[1], 10, 64)
	seq, _ := strconv.ParseUint(reSeq.FindStringSubmatch(script)[1], 10, 64)
	chain := reChain.FindStringSubmatch(script)[1]

	priv, _ := hex.DecodeString(TestKeyHex)
	pk := secp256k1.PrivKeyFromBytes(priv)
	var pkv []byte
	pkv = protowire.AppendTag(pkv, 1, protowire.BytesType)
	pkv = protowire.AppendBytes(pkv, pk.PubKey().SerializeCompressed())
	ai := &txv1beta1.AuthInfo{Fee: fee, SignerInfos: []*txv1beta1.SignerInfo{{
		PublicKey: &anypb.Any{TypeUrl: "/cosmos.evm.crypto.v1.ethsecp256k1.PubKey", Value: pkv},
		ModeInfo:  &txv1beta1.ModeInfo{Sum: &txv1beta1.ModeInfo_Single_{Single: &txv1beta1.ModeInfo_Single{Mode: signingv1beta1.SignMode_SIGN_MODE_DIRECT}}},
		Sequence:  seq,
	}}}
	bodyB, _ := proto.Marshal(body)
	aiB, _ := proto.Marshal(ai)
	sd, _ := proto.Marshal(&txv1beta1.SignDoc{BodyBytes: bodyB, AuthInfoBytes: aiB, ChainId: chain, AccountNumber: num})
	h := sha3.NewLegacyKeccak256()
	h.Write(sd)
	sig := ecdsa.Sign(pk, h.Sum(nil))
	r, s := sig.R(), sig.S()
	rb, sb := r.Bytes(), s.Bytes()
	return proto.Marshal(&txv1beta1.TxRaw{BodyBytes: bodyB, AuthInfoBytes: aiB, Signatures: [][]byte{append(rb[:], sb[:]...)}})
}

// TestPubKey is the compressed public key of TestKeyHex.
func TestPubKey() []byte {
	priv, _ := hex.DecodeString(TestKeyHex)
	return secp256k1.PrivKeyFromBytes(priv).PubKey().SerializeCompressed()
}
