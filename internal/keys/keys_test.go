package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

const testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

// Known-answer: the classic BIP-39 test mnemonic at m/44'/60'/0'/0/0 derives
// ETH address 0x9858EfFD232B4033E47d90003D41EC34EcaEda94 (eth_secp256k1).
func TestDeriveEthSecp256k1_KAT(t *testing.T) {
	priv, err := Derive(testMnemonic, AlgoEthSecp256k1, 60, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	addr := AddressFor(AlgoEthSecp256k1, priv.PubKey())
	got := "0x" + hex.EncodeToString(addr)
	want := "0x9858effd232b4033e47d90003d41ec34ecaeda94"
	if got != want {
		t.Fatalf("address = %s, want %s", got, want)
	}
}

func TestBech32RoundTrip(t *testing.T) {
	priv, _ := Derive(testMnemonic, AlgoEthSecp256k1, 60, 0, 0)
	k := &Key{Address: AddressFor(AlgoEthSecp256k1, priv.PubKey())}
	b, err := k.Bech32("cosmos")
	if err != nil {
		t.Fatal(err)
	}
	back, err := Bech32ToHex(b)
	if err != nil {
		t.Fatal(err)
	}
	if back != k.Hex() {
		t.Fatalf("roundtrip mismatch: %s vs %s", back, k.Hex())
	}
}

func TestValoperPrefix(t *testing.T) {
	priv, _ := Derive(testMnemonic, AlgoEthSecp256k1, 60, 0, 0)
	k := &Key{Address: AddressFor(AlgoEthSecp256k1, priv.PubKey())}
	acct, _ := k.Bech32("cosmos")
	val, err := ValAddress(acct)
	if err != nil {
		t.Fatal(err)
	}
	// same bytes, different hrp
	a1, _ := Bech32ToHex(acct)
	a2, _ := Bech32ToHex(val)
	if a1 != a2 {
		t.Fatal("valoper should share address bytes")
	}
	if got := val[:13]; got != "cosmosvaloper" {
		t.Fatalf("valoper prefix = %s", got)
	}
}

// verify parses a 64-byte r||s signature and checks it against a digest.
func verify(t *testing.T, sig []byte, digest []byte, pub *secp256k1.PublicKey) bool {
	t.Helper()
	r := new(secp256k1.ModNScalar)
	s := new(secp256k1.ModNScalar)
	if overflow := r.SetByteSlice(sig[:32]); overflow {
		t.Fatal("r overflows curve order")
	}
	if overflow := s.SetByteSlice(sig[32:]); overflow {
		t.Fatal("s overflows curve order")
	}
	return ecdsa.NewSignature(r, s).Verify(digest, pub)
}

func TestSignVerify(t *testing.T) {
	priv, err := Derive(testMnemonic, AlgoSecp256k1, 118, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	k := &Key{Algo: AlgoSecp256k1, priv: priv, PubKey: priv.PubKey().SerializeCompressed()}
	msg := []byte("hello")
	sig := k.Sign(msg)
	if len(sig) != 64 {
		t.Fatalf("sig len = %d, want 64 (r||s)", len(sig))
	}
	h := sha256.Sum256(msg)
	if !verify(t, sig, h[:], priv.PubKey()) {
		t.Fatal("secp256k1 signature does not verify against sha256(msg)")
	}
}

// eth_secp256k1 must sign keccak256(msg) — the chain rejects sha256 digests
// (verified live: signature-verification failures on primium-1).
func TestSignEthSecp256k1_UsesKeccak(t *testing.T) {
	priv, err := Derive(testMnemonic, AlgoEthSecp256k1, 60, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	k := &Key{Algo: AlgoEthSecp256k1, priv: priv, PubKey: priv.PubKey().SerializeCompressed()}
	msg := []byte("cometcli sign kat")
	sig := k.Sign(msg)

	kh := sha3.NewLegacyKeccak256()
	kh.Write(msg)
	if !verify(t, sig, kh.Sum(nil), priv.PubKey()) {
		t.Fatal("signature does not verify against keccak256(msg)")
	}
	wrong := sha256.Sum256(msg)
	if verify(t, sig, wrong[:], priv.PubKey()) {
		t.Fatal("signature unexpectedly verifies against sha256(msg)")
	}
}

// RFC6979 makes signing deterministic — freeze the exact bytes so a change
// in the digest or serialization can't slip through silently.
func TestSignGolden_KAT(t *testing.T) {
	msg := []byte("cometcli sign kat")

	priv, _ := Derive(testMnemonic, AlgoEthSecp256k1, 60, 0, 0)
	k := &Key{Algo: AlgoEthSecp256k1, priv: priv, PubKey: priv.PubKey().SerializeCompressed()}
	want := "055206149a819c48b7cd8dc257bacfa394ab292560daa0f429f5f9ef6a1450fa77c162831ffb4bc79a5398c942756734a1da27b605e134cc9630f089f7d3aac7"
	if got := hex.EncodeToString(k.Sign(msg)); got != want {
		t.Fatalf("eth_secp256k1 sig = %s, want %s", got, want)
	}

	priv2, _ := Derive(testMnemonic, AlgoSecp256k1, 118, 0, 0)
	k2 := &Key{Algo: AlgoSecp256k1, priv: priv2, PubKey: priv2.PubKey().SerializeCompressed()}
	want2 := "0b89130fcca7338d85b07f0e0b6923473ffb8d520ab347f5010939b689e1843a0fe83b6b5a6956ef9ec86633918c26b1a790662d6630f58e371411cb9432f87c"
	if got := hex.EncodeToString(k2.Sign(msg)); got != want2 {
		t.Fatalf("secp256k1 sig = %s, want %s", got, want2)
	}
}
