// Package keystool implements keys.* tools for the ops keyring.
package keystool

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/keys"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Register adds all keys.* tools.
func Register(r *toolkit.Registry) {
	r.Register(addTool{})
	r.Register(listTool{})
	r.Register(showTool{})
	r.Register(rmTool{})
	r.Register(convertTool{})
}

// openRing opens the profile keyring, defaulting to a throwaway profile.
func openRing(c *toolkit.Context) (*keys.Ring, error) {
	p := c.Profile
	if p == nil {
		p = &config.Profile{Signer: config.Signer{Backend: "os"}}
	}
	return keys.Open(p)
}

type addTool struct{}

func (addTool) Name() string { return "keys.add" }
func (addTool) Desc() string {
	return "Add an ops key: --recover to import a mnemonic, else generates one"
}
func (addTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"name":       toolkit.Str("key name"),
		"recover":    toolkit.Bool("prompt for an existing mnemonic instead of generating"),
		"algo":       toolkit.Enum("key algorithm", "eth_secp256k1", "secp256k1"),
		"coin-type":  toolkit.Int("HD coin type (default 60)"),
		"account":    toolkit.Int("HD account (default 0)"),
		"index":      toolkit.Int("HD index (default 0)"),
		"show-mnemonic": toolkit.Bool("print the generated mnemonic (write it down!)"),
	}, "name")
}
func (addTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (addTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	name := a.String("name", "")
	ring, err := openRing(c)
	if err != nil {
		return nil, err
	}
	var mnemonic string
	if a.Bool("recover", false) {
		fmt.Fprint(c.Out, "Enter BIP-39 mnemonic: ")
		reader := bufio.NewReader(os.Stdin)
		mnemonic, _ = reader.ReadString('\n')
		mnemonic = strings.TrimSpace(mnemonic)
	}
	algo := keys.Algo(a.String("algo", string(keys.AlgoEthSecp256k1)))
	recovered := mnemonic != ""
	k, mnemonic, err := ring.Generate(name, mnemonic, algo,
		uint32(a.Int("coin-type", int64(keys.DefaultCoinType))),
		uint32(a.Int("account", 0)), uint32(a.Int("index", 0)))
	if err != nil {
		return nil, err
	}
	prefix := "cosmos"
	if c.Profile != nil && c.Profile.Bech32Prefix != "" {
		prefix = c.Profile.Bech32Prefix
	}
	addr, err := k.Bech32(prefix)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "key:     %s\nalgo:    %s\naddress: %s\nhex:     %s\n", name, algo, addr, k.Hex())
	if !recovered {
		fmt.Fprintf(&b, "\nWRITE THIS DOWN — mnemonic (never stored anywhere but the keyring):\n%s\n", mnemonic)
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{
		"name": name, "address": addr, "hex": k.Hex(), "algo": string(algo),
	}}, nil
}

type listTool struct{}

func (listTool) Name() string { return "keys.list" }
func (listTool) Desc() string {
	return "List ops keys with addresses"
}
func (listTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (listTool) Tier() toolkit.Tier     { return toolkit.TierObserve }

func (listTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	ring, err := openRing(c)
	if err != nil {
		return nil, err
	}
	names, err := ring.List()
	if err != nil {
		return nil, err
	}
	prefix := "cosmos"
	if c.Profile != nil && c.Profile.Bech32Prefix != "" {
		prefix = c.Profile.Bech32Prefix
	}
	var b strings.Builder
	var out []map[string]any
	for _, n := range names {
		k, err := ring.Get(n)
		if err != nil {
			fmt.Fprintf(&b, "%-20s <error: %v>\n", n, err)
			continue
		}
		addr, _ := k.Bech32(prefix)
		fmt.Fprintf(&b, "%-20s %s  %s\n", n, addr, k.Hex())
		out = append(out, map[string]any{"name": n, "address": addr, "hex": k.Hex()})
	}
	if len(names) == 0 {
		fmt.Fprintln(&b, "no keys — `cometcli keys add <name>`")
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"keys": out}}, nil
}

type showTool struct{}

func (showTool) Name() string { return "keys.show" }
func (showTool) Desc() string {
	return "Show one key's addresses (bech32 + hex)"
}
func (showTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"name": toolkit.Str("key name"),
	}, "name")
}
func (showTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (showTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	ring, err := openRing(c)
	if err != nil {
		return nil, err
	}
	k, err := ring.Get(a.String("name", ""))
	if err != nil {
		return nil, err
	}
	prefix := "cosmos"
	if c.Profile != nil && c.Profile.Bech32Prefix != "" {
		prefix = c.Profile.Bech32Prefix
	}
	addr, err := k.Bech32(prefix)
	if err != nil {
		return nil, err
	}
	valoper, _ := keys.ValAddress(addr)
	t := fmt.Sprintf("name:    %s\nalgo:    %s\naddress: %s\nvaloper: %s\nhex:     %s\n",
		k.Name, k.Algo, addr, valoper, k.Hex())
	return &toolkit.Result{Text: t, Data: map[string]any{
		"name": k.Name, "address": addr, "valoper": valoper, "hex": k.Hex(), "algo": string(k.Algo),
	}}, nil
}

type rmTool struct{}

func (rmTool) Name() string { return "keys.rm" }
func (rmTool) Desc() string {
	return "Delete an ops key from the keyring"
}
func (rmTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{"name": toolkit.Str("key name")}, "name")
}
func (rmTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

func (rmTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	name := a.String("name", "")
	if err := c.Approve("delete ops key "+name, toolkit.TierLocalChange, nil); err != nil {
		return nil, err
	}
	ring, err := openRing(c)
	if err != nil {
		return nil, err
	}
	if err := ring.Remove(name); err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: "deleted " + name, Data: map[string]any{"deleted": name}}, nil
}

type convertTool struct{}

func (convertTool) Name() string { return "keys.convert" }
func (convertTool) Desc() string {
	return "Convert an address: bech32 ↔ 0x hex"
}
func (convertTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"address": toolkit.Str("bech32 or 0x address"),
		"prefix":  toolkit.Str("bech32 prefix for hex→bech32 (default: profile)"),
	}, "address")
}
func (convertTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (convertTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	addr := a.String("address", "")
	var b strings.Builder
	if strings.HasPrefix(addr, "0x") {
		prefix := a.String("prefix", "")
		if prefix == "" && c.Profile != nil {
			prefix = c.Profile.Bech32Prefix
		}
		if prefix == "" {
			prefix = "cosmos"
		}
		var raw []byte
		fmt.Sscanf(addr, "0x%x", &raw)
		k := keys.Key{Address: raw}
		bech, err := k.Bech32(prefix)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "bech32: %s\nhex:    %s\n", bech, addr)
	} else {
		hex, err := keys.Bech32ToHex(addr)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "bech32: %s\nhex:    %s\n", addr, hex)
	}
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"input": addr}}, nil
}
