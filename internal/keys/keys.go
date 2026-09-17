// Package keys implements the cometcli ops keyring: named signing keys stored
// in the OS keychain (or an encrypted file/test backend), used for on-chain
// ops transactions. It deliberately has NO code path for consensus key
// material (priv_validator_key.json is never touched).
//
// Default algorithm is eth_secp256k1 — the key type cosmos-evm chains use:
// address = keccak256(uncompressed_pubkey)[12:]. Standard cosmos secp256k1
// (address = sha256(compressed_pubkey)[:20]) is also supported.
package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/99designs/keyring"
	"github.com/cosmos/btcutil/bech32"
	"github.com/cosmos/go-bip39"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	bip32 "github.com/tyler-smith/go-bip32"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // hash160 is required by cosmos address derivation
	"golang.org/x/crypto/sha3"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// Algo is the signing/address algorithm.
type Algo string

const (
	AlgoEthSecp256k1 Algo = "eth_secp256k1" // cosmos-evm default
	AlgoSecp256k1    Algo = "secp256k1"     // standard cosmos
)

// DefaultCoinType for EVM chains is 60 (ETH). Cosmos chains use 118.
const DefaultCoinType uint32 = 60

// Key is a derived signing key.
type Key struct {
	Name    string
	Algo    Algo
	PubKey  []byte // compressed secp256k1
	Address []byte // 20 bytes
	priv    *secp256k1.PrivateKey
}

// Bech32 renders the address with a prefix.
func (k *Key) Bech32(prefix string) (string, error) {
	conv, err := bech32.ConvertBits(k.Address, 8, 5, true)
	if err != nil {
		return "", err
	}
	return bech32.Encode(prefix, conv)
}

// Hex renders the address as an EIP-55-style 0x string (lowercase; the
// checksum form is cosmetic).
func (k *Key) Hex() string { return "0x" + fmt.Sprintf("%x", k.Address) }

// ValAddress converts an account address to the valoper form (same bytes,
// different prefix: <prefix>valoper).
func ValAddress(accountBech32 string) (string, error) {
	prefix, data, err := bech32.DecodeNoLimit(accountBech32)
	if err != nil {
		return "", err
	}
	conv, err := bech32.ConvertBits(data, 5, 8, false)
	if err != nil {
		return "", err
	}
	out, err := bech32.ConvertBits(conv, 8, 5, true)
	if err != nil {
		return "", err
	}
	return bech32.Encode(prefix+"valoper", out)
}

// ConsAddress renders consensus pubkey bytes (ed25519 addr = sha256(pk)[:20])
// into <prefix>valcons form.
func ConsAddress(addr []byte, prefix string) (string, error) {
	conv, err := bech32.ConvertBits(addr, 8, 5, true)
	if err != nil {
		return "", err
	}
	return bech32.Encode(prefix+"valcons", conv)
}

// Bech32ToHex converts a bech32 account address to 0x hex.
func Bech32ToHex(addr string) (string, error) {
	_, data, err := bech32.DecodeNoLimit(addr)
	if err != nil {
		return "", err
	}
	conv, err := bech32.ConvertBits(data, 5, 8, false)
	if err != nil {
		return "", err
	}
	return "0x" + fmt.Sprintf("%x", conv), nil
}

// Ring is the ops keyring.
type Ring struct {
	kr      keyring.Keyring
	backend string
}

// Open opens the keyring backend selected by the profile.
func Open(p *config.Profile) (*Ring, error) {
	backend := "os"
	if p != nil && p.Signer.Backend != "" {
		backend = p.Signer.Backend
	}
	dir, err := config.Path("keys")
	if err != nil {
		return nil, err
	}
	if backend == "test" || backend == "memory" {
		return &Ring{kr: keyring.NewArrayKeyring(nil), backend: backend}, nil
	}
	var allowed []keyring.BackendType
	if backend == "file" {
		allowed = []keyring.BackendType{keyring.FileBackend}
	} else if backend != "os" {
		return nil, fmt.Errorf("unknown signer backend %q", backend)
	}
	kr, err := keyring.Open(keyring.Config{
		ServiceName:              "cometcli",
		AllowedBackends:          allowed,
		FileDir:                  dir,
		FilePasswordFunc:         filePass,
		KeychainTrustApplication: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open keyring: %w", err)
	}
	return &Ring{kr: kr, backend: backend}, nil
}

func filePass(prompt string) (string, error) {
	if pw := os.Getenv("COMETCLI_KEYRING_PASSWORD"); pw != "" {
		return pw, nil
	}
	return "", errors.New("file backend requires COMETCLI_KEYRING_PASSWORD env var")
}

// List returns key names.
func (r *Ring) List() ([]string, error) { return r.kr.Keys() }

// stored record: "algo|mnemonic_or_privhex|coinType|account|index"
func (r *Ring) put(name string, rec string) error {
	return r.kr.Set(keyring.Item{Key: name, Data: []byte(rec)})
}

// Generate creates a key from a mnemonic (generated if empty) and stores it.
// Returns the key and the mnemonic (the generated one, if any).
func (r *Ring) Generate(name, mnemonic string, algo Algo, coinType, account, index uint32) (*Key, string, error) {
	if mnemonic == "" {
		ent, err := bip39.NewEntropy(256)
		if err != nil {
			return nil, "", err
		}
		mnemonic, err = bip39.NewMnemonic(ent)
		if err != nil {
			return nil, "", err
		}
	} else if !bip39.IsMnemonicValid(mnemonic) {
		return nil, "", errors.New("invalid BIP-39 mnemonic")
	}
	if coinType == 0 {
		coinType = DefaultCoinType
	}
	rec := fmt.Sprintf("%s|%s|%d|%d|%d", algo, mnemonic, coinType, account, index)
	if err := r.put(name, rec); err != nil {
		return nil, "", err
	}
	k, err := r.derive(name, rec, mnemonic)
	return k, mnemonic, err
}

// ImportHex stores a raw secp256k1 private key (64 hex chars, no 0x).
// Recorded as "algo|hex:<hex>|0|0|0" — no derivation needed on load.
func (r *Ring) ImportHex(name, hexkey string, algo Algo) (*Key, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(hexkey), "0x"))
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("invalid secp256k1 private key hex")
	}
	rec := fmt.Sprintf("%s|hex:%s|%d|%d|%d", algo, hex.EncodeToString(raw), 0, 0, 0)
	if err := r.put(name, rec); err != nil {
		return nil, err
	}
	priv := secp256k1.PrivKeyFromBytes(raw)
	k := &Key{Name: name, Algo: algo, priv: priv, PubKey: priv.PubKey().SerializeCompressed()}
	k.Address = AddressFor(algo, priv.PubKey())
	return k, nil
}

// Get loads and derives a stored key.
func (r *Ring) Get(name string) (*Key, error) {
	item, err := r.kr.Get(name)
	if err != nil {
		return nil, fmt.Errorf("key %q: %w", name, err)
	}
	rec := string(item.Data)
	parts := strings.SplitN(rec, "|", 5)
	if len(parts) != 5 {
		return nil, fmt.Errorf("key %q has corrupt record", name)
	}
	return r.derive(name, rec, parts[1])
}

func (r *Ring) derive(name, rec, mnemonic string) (*Key, error) {
	parts := strings.SplitN(rec, "|", 5)
	algo := Algo(parts[0])
	var coin, account, index uint32
	fmt.Sscanf(parts[2], "%d", &coin)
	fmt.Sscanf(parts[3], "%d", &account)
	fmt.Sscanf(parts[4], "%d", &index)
	var priv *secp256k1.PrivateKey
	var err error
	if hexPart, ok := strings.CutPrefix(parts[1], "hex:"); ok {
		var raw []byte
		raw, err = hex.DecodeString(hexPart)
		if err == nil && len(raw) == 32 {
			priv = secp256k1.PrivKeyFromBytes(raw)
		} else {
			err = fmt.Errorf("key %q has corrupt privhex record", name)
		}
	} else {
		priv, err = Derive(mnemonic, algo, coin, account, index)
	}
	if err != nil {
		return nil, err
	}
	k := &Key{Name: name, Algo: algo, priv: priv, PubKey: priv.PubKey().SerializeCompressed()}
	k.Address = AddressFor(algo, priv.PubKey())
	return k, nil
}

// Remove deletes a key.
func (r *Ring) Remove(name string) error { return r.kr.Remove(name) }

// Derive runs BIP-39 → BIP-32 m/44'/coin'/account'/0/index → secp256k1 key.
func Derive(mnemonic string, algo Algo, coinType, account, index uint32) (*secp256k1.PrivateKey, error) {
	seed := bip39.NewSeed(mnemonic, "")
	master, err := bip32.NewMasterKey(seed)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("m/44'/%d'/%d'/0/%d", coinType, account, index)
	node := master
	for _, part := range strings.Split(strings.TrimPrefix(path, "m/"), "/") {
		hardened := strings.HasSuffix(part, "'")
		var n uint32
		fmt.Sscanf(strings.TrimSuffix(part, "'"), "%d", &n)
		if hardened {
			n += bip32.FirstHardenedChild
		}
		if node, err = node.NewChildKey(n); err != nil {
			return nil, fmt.Errorf("derive %s: %w", path, err)
		}
	}
	return secp256k1.PrivKeyFromBytes(node.Key), nil
}

// AddressFor computes the account address per algorithm.
func AddressFor(algo Algo, pub *secp256k1.PublicKey) []byte {
	switch algo {
	case AlgoEthSecp256k1:
		h := sha3.NewLegacyKeccak256()
		h.Write(pub.SerializeUncompressed()[1:]) // drop 0x04
		return h.Sum(nil)[12:]
	default: // cosmos secp256k1: sha256(compressed)[:20]
		sum := sha256.Sum256(pub.SerializeCompressed())
		r := ripemd160.New()
		r.Write(sum[:])
		return r.Sum(nil)[:20]
	}
}

// Sign produces a 64-byte r||s secp256k1 signature, matching cosmos-sdk
// semantics per key type: eth_secp256k1 signs keccak256(msg), the cosmos
// default secp256k1 signs sha256(msg).
func (k *Key) Sign(msg []byte) []byte {
	var h [32]byte
	if k.Algo == AlgoEthSecp256k1 {
		kh := sha3.NewLegacyKeccak256()
		kh.Write(msg)
		copy(h[:], kh.Sum(nil))
	} else {
		h = sha256.Sum256(msg)
	}
	sig := ecdsa.Sign(k.priv, h[:])
	r, s := sig.R(), sig.S()
	rb, sb := r.Bytes(), s.Bytes()
	out := make([]byte, 64)
	copy(out[:32], rb[:])
	copy(out[32:], sb[:])
	return out
}
