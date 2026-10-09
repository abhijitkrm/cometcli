package networktool

import (
	"encoding/json"
	"fmt"
	"math/big"
	"path"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// GenesisNode is a genesis validator as the coordinator knows it: only
// public facts — its keys stay in its home on its host.
type GenesisNode struct {
	Ctx     *toolkit.Context
	Home    string // absolute, on its host
	User    string // uid:gid owning Home
	Moniker string
	Account string // operator account (bech32)
	NodeID  string
	Image   string
}

// evmdIn runs evmd in the image against a home, as the home's owner.
func evmdIn(user, home, image string) string {
	return fmt.Sprintf("docker run --rm -i --user %s -e HOME=%s -w %s -v %s:%s --entrypoint evmd %s",
		user, containerHome, containerHome, common.ShellQ(home), containerHome, common.ShellQ(image))
}

// ImageFor is the image a spec runs.
func ImageFor(spec *netspec.Spec) string {
	if spec.Image.Naming != "" {
		return strings.NewReplacer("{tag}", spec.Image.Tag, "{version}", strings.TrimPrefix(spec.Image.Tag, "v")).Replace(spec.Image.Naming)
	}
	return spec.Image.Name + ":" + spec.Image.Tag
}

// prepareHome resolves and checks a node home on its host: absolute,
// writable, and not already holding a validator.
func prepareHome(c *toolkit.Context, h host.Host, home string) (abs, user string, hasKey bool, err error) {
	out, _, _ := run(c, h, fmt.Sprintf(`h=%s; case "$h" in "~"|"~/"*) h="$HOME${h#\~}";; esac; mkdir -p "$h" 2>/dev/null; if [ -w "$h" ]; then echo "ok $h $(id -u):$(id -g)"; else echo "denied $h $(id -un)"; fi; test -f "$h/config/priv_validator_key.json" && echo has-key || true`, shellHome(home)), time.Minute)
	f := strings.Fields(out)
	if len(f) < 3 || f[0] != "ok" {
		who := "you"
		if len(f) >= 3 {
			who = f[2]
		}
		return "", "", false, fmt.Errorf("%s isn't writable on %s — create it for %s once: sudo mkdir -p %s && sudo chown -R %s %s", home, c.Profile.Name, who, home, who, home)
	}
	return f[1], f[2], strings.Contains(out, "has-key"), nil
}

// KeyringOpts says where validator operator keys live and how they're
// unlocked.
type KeyringOpts struct {
	Backend  string // test | file
	Password string // file backend
}

func (k KeyringOpts) stdin(times int) []byte {
	if k.Backend != "file" {
		return nil
	}
	return []byte(strings.Repeat(k.Password+"\n", times))
}

// PrepareValidator initialises a genesis validator on its own host: node
// and consensus keys from init, the operator key in the node's own
// keyring. It returns the node's public facts and the operator mnemonic,
// which the caller shows to the operator only.
func PrepareValidator(c *toolkit.Context, spec *netspec.Spec, home string, kr KeyringOpts) (*GenesisNode, string, error) {
	h, err := c.Host()
	if err != nil {
		return nil, "", err
	}
	image := ImageFor(spec)
	if _, code, _ := run(c, h, "docker image inspect "+common.ShellQ(image)+" >/dev/null 2>&1", time.Minute); code != 0 {
		return nil, "", fmt.Errorf("image %s isn't on %s — build it there first: cometcli --profile %s upgrade build --tag %s", image, c.Profile.Name, c.Profile.Name, spec.Image.Tag)
	}
	abs, user, hasKey, err := prepareHome(c, h, home)
	if err != nil {
		return nil, "", err
	}
	if hasKey {
		return nil, "", fmt.Errorf("%s on %s already holds a node (config/priv_validator_key.json) — a new genesis needs fresh homes; move it aside yourself if it's really unused", abs, c.Profile.Name)
	}
	n := &GenesisNode{Ctx: c, Home: abs, User: user, Moniker: c.Profile.Name, Image: image}
	ev := evmdIn(user, abs, image)
	if out, code, err := run(c, h, fmt.Sprintf("%s init %s --chain-id %s --home %s >/dev/null 2>&1 && echo ok", ev, common.ShellQ(n.Moniker), common.ShellQ(spec.Chain.ID), containerHome), 3*time.Minute); err != nil || code != 0 || !strings.Contains(out, "ok") {
		return nil, "", fmt.Errorf("init on %s failed: %v %s", c.Profile.Name, err, out)
	}
	// the operator key, created in the node's keyring; --output json
	// carries the mnemonic, which only the caller sees
	res, err := host.ExecIn(c, h, fmt.Sprintf("%s keys add %s --keyring-backend %s --home %s --output json 2>/dev/null", ev, common.ShellQ(n.Moniker), kr.Backend, containerHome), kr.stdin(2), 1<<20)
	if err != nil || res.Code != 0 {
		return nil, "", fmt.Errorf("creating the operator key on %s failed (exit %d)", c.Profile.Name, res.Code)
	}
	var key struct {
		Address  string `json:"address"`
		Mnemonic string `json:"mnemonic"`
	}
	if err := json.Unmarshal([]byte(lastJSON(res.Output)), &key); err != nil || key.Address == "" {
		return nil, "", fmt.Errorf("reading the new operator key on %s: unexpected output", c.Profile.Name)
	}
	n.Account = key.Address
	id, _, _ := run(c, h, ev+" comet show-node-id --home "+containerHome+" 2>/dev/null | tail -1", time.Minute)
	n.NodeID = strings.TrimSpace(id)
	if n.NodeID == "" {
		return nil, "", fmt.Errorf("reading the node id on %s", c.Profile.Name)
	}
	return n, key.Mnemonic, nil
}

// lastJSON finds the JSON object in output that may have other lines.
func lastJSON(s string) string {
	if i := strings.LastIndex(s, "\n{"); i >= 0 {
		return strings.TrimSpace(s[i:])
	}
	if i := strings.Index(s, "{"); i >= 0 {
		return strings.TrimSpace(s[i:])
	}
	return ""
}

// Account is an extra genesis account.
type Account struct {
	Address string // bech32 or 0x
	Amount  string // in the bond denom
}

// BuildBase makes the base genesis on the builder's host: evmd init, the
// spec's parameters, every validator's balance and the extra accounts.
func BuildBase(c *toolkit.Context, spec *netspec.Spec, nodes []*GenesisNode, extra []Account) ([]byte, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	image := ImageFor(spec)
	dir, user, _, err := prepareHome(c, h, "~/.cometcli-build/genesis-"+spec.Chain.ID)
	if err != nil {
		return nil, err
	}
	ev := evmdIn(user, dir, image)
	if _, code, _ := run(c, h, fmt.Sprintf("rm -rf %s/config %s/data && %s init builder --chain-id %s --home %s >/dev/null 2>&1", common.ShellQ(dir), common.ShellQ(dir), ev, common.ShellQ(spec.Chain.ID), containerHome), 3*time.Minute); code != 0 {
		return nil, fmt.Errorf("builder init failed on %s", c.Profile.Name)
	}
	gp := path.Join(dir, "config", "genesis.json")
	raw, err := h.ReadFile(c, gp)
	if err != nil {
		return nil, err
	}
	patched, err := spec.PatchGenesis(raw)
	if err != nil {
		return nil, err
	}
	if err := h.WriteFile(c, gp, patched, 0o644); err != nil {
		return nil, err
	}
	balance := spec.Genesis.InitialBalance
	if balance == "" {
		return nil, fmt.Errorf("the spec has no genesis.initial_balance for validators")
	}
	add := func(addr, amount string) error {
		out, code, _ := run(c, h, fmt.Sprintf("%s genesis add-genesis-account %s %s%s --home %s 2>&1 | tail -1", ev, common.ShellQ(addr), amount, spec.Chain.Denom, containerHome), time.Minute)
		if code != 0 || strings.Contains(strings.ToLower(out), "error") {
			return fmt.Errorf("add-genesis-account %s: %s", addr, strings.TrimSpace(out))
		}
		return nil
	}
	for _, n := range nodes {
		if err := add(n.Account, balance); err != nil {
			return nil, err
		}
	}
	for _, a := range extra {
		if err := add(a.Address, a.Amount); err != nil {
			return nil, err
		}
	}
	return h.ReadFile(c, gp)
}

// Gentx signs a validator's genesis transaction on its own host, against
// the base genesis; only the gentx comes back.
func Gentx(n *GenesisNode, spec *netspec.Spec, base []byte, kr KeyringOpts) ([]byte, error) {
	c := n.Ctx
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	if err := h.WriteFile(c, path.Join(n.Home, "config", "genesis.json"), base, 0o644); err != nil {
		return nil, err
	}
	stake := spec.Genesis.InitialStake
	if stake == "" {
		return nil, fmt.Errorf("the spec has no genesis.initial_stake")
	}
	fee := ""
	if gp, ok := new(big.Int).SetString(spec.MinGasPrice, 10); ok {
		fee = "--fees " + new(big.Int).Mul(gp, big.NewInt(200000)).String() + spec.GasDenom()
	}
	ev := evmdIn(n.User, n.Home, n.Image)
	script := fmt.Sprintf("rm -rf %[1]s/config/gentx && %[2]s genesis gentx %[3]s %[4]s%[5]s --keyring-backend %[6]s --chain-id %[7]s %[8]s --home %[9]s >/dev/null 2>&1 && cat %[1]s/config/gentx/*.json",
		common.ShellQ(n.Home), ev, common.ShellQ(n.Moniker), stake, spec.Chain.Denom, kr.Backend, common.ShellQ(spec.Chain.ID), fee, containerHome)
	res, err := host.ExecIn(c, h, script, kr.stdin(1), 1<<20)
	if err != nil || res.Code != 0 {
		return nil, fmt.Errorf("gentx on %s failed (exit %d)", c.Profile.Name, res.Code)
	}
	return []byte(res.Output), nil
}

// Collect assembles the final genesis on the builder's host from the base
// and every gentx, and validates it — strictly: a new network's genesis
// must pass.
func Collect(c *toolkit.Context, spec *netspec.Spec, gentxs map[string][]byte) ([]byte, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	dir, user, _, err := prepareHome(c, h, "~/.cometcli-build/genesis-"+spec.Chain.ID)
	if err != nil {
		return nil, err
	}
	if _, code, _ := run(c, h, "rm -rf "+common.ShellQ(dir+"/config/gentx")+" && mkdir -p "+common.ShellQ(dir+"/config/gentx"), time.Minute); code != 0 {
		return nil, fmt.Errorf("preparing gentx dir")
	}
	for name, g := range gentxs {
		if err := h.WriteFile(c, path.Join(dir, "config", "gentx", "gentx-"+name+".json"), g, 0o644); err != nil {
			return nil, err
		}
	}
	ev := evmdIn(user, dir, ImageFor(spec))
	if out, code, _ := run(c, h, ev+" genesis collect-gentxs --home "+containerHome+" 2>&1 | tail -2", 3*time.Minute); code != 0 || strings.Contains(out, "rror") {
		return nil, fmt.Errorf("collect-gentxs: %s", strings.TrimSpace(out))
	}
	if out, code, _ := run(c, h, ev+" genesis validate-genesis --home "+containerHome+" 2>&1", 2*time.Minute); code != 0 || !strings.Contains(out, "valid genesis") {
		return nil, fmt.Errorf("the new genesis doesn't validate: %s", strings.TrimSpace(out))
	}
	return h.ReadFile(c, path.Join(dir, "config", "genesis.json"))
}
