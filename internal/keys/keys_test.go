package keys

import (
	"encoding/hex"
	"testing"
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

func TestSignVerify(t *testing.T) {
	priv, err := Derive(testMnemonic, AlgoSecp256k1, 118, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	k := &Key{priv: priv, PubKey: priv.PubKey().SerializeCompressed()}
	sig := k.Sign([]byte("hello"))
	if len(sig) != 64 {
		t.Fatalf("sig len = %d, want 64 (r||s)", len(sig))
	}
}
