package networktool

// Workflow B: a genesis with validators the coordinator can't reach.
// Outside validators make their keys on their own (ideally offline)
// machines and exchange only public files with the coordinator:
//
//	validator:   genesis join     → <moniker>.join.json   (account, node id, consensus pubkey)
//	coordinator: genesis base     → kit/                  (base genesis funding every account)
//	validator:   genesis gentx    → <moniker>.gentx.json  (gentx signed against the kit)
//	coordinator: genesis collect  → packages/<moniker>/   (final genesis + every peer)
//	validator:   genesis install  → node started
//
// Each file carries checksums and is verified where it arrives. Keys and
// mnemonics never leave the key machine: they're made inside a container
// with no network, and a new mnemonic goes to a 0600 file on that machine.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/keys"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// FormatVersion is the version of the join, kit, gentx and package files.
const FormatVersion = 1

// Offline records whether the key machine had a network when keys were
// made or a gentx signed.
type Offline struct {
	Checked bool   `json:"checked"`
	Online  bool   `json:"online"`
	Detail  string `json:"detail,omitempty"`
}

// CheckOffline looks for a default route or an up interface with a global
// address on the machine. Docker's own bridges don't count: every machine
// with docker has them, connected or not.
func CheckOffline(c *toolkit.Context, h host.Host) Offline {
	out, _, _ := run(c, h, `{ ip route show default 2>/dev/null; ip -o addr show up scope global 2>/dev/null | awk '$2 !~ /^(docker|br-|veth|virbr|cni|flannel|lo)/ {print "if " $2 " " $4}'; } || true
if ! command -v ip >/dev/null 2>&1; then route -n get default 2>/dev/null | awk '/interface:/{print "route via " $2}'; fi`, 30*time.Second)
	out = strings.TrimSpace(out)
	if out == "" {
		return Offline{Checked: true}
	}
	lines := strings.Split(out, "\n")
	return Offline{Checked: true, Online: true, Detail: strings.Join(lines[:min(len(lines), 3)], "; ")}
}

// offlineGate refuses a connected key machine unless allowed, in which
// case the result is marked as made online.
func offlineGate(c *toolkit.Context, h host.Host, allowOnline bool) (Offline, error) {
	o := CheckOffline(c, h)
	if o.Online && !allowOnline {
		return o, fmt.Errorf("this machine is online (%s) — validator keys should be made with no network: disconnect it and run again, or pass --allow-online (the result is marked as made online)", o.Detail)
	}
	return o, nil
}

// evmdOffline runs evmd in the image with no network at all.
func evmdOffline(user, home, image string) string {
	return strings.Replace(evmdIn(user, home, image), "docker run --rm -i", "docker run --rm -i --network none", 1)
}

// --- join ------------------------------------------------------------------

// Join is what a validator sends before the base genesis exists: public
// facts only.
type Join struct {
	Version int     `json:"version"`
	ChainID string  `json:"chain_id"`
	Moniker string  `json:"moniker"`
	Account string  `json:"account"`
	NodeID  string  `json:"node_id"`
	ConsKey string  `json:"consensus_pubkey"`
	IP      string  `json:"ip"`
	P2PPort int     `json:"p2p_port"`
	Offline Offline `json:"offline"`
}

// JoinOpts are a validator's choices for genesis join.
type JoinOpts struct {
	Home        string
	Moniker     string
	IP          string
	P2PPort     int
	Keyring     KeyringOpts
	RecoverFrom string // a mnemonic file on the key machine (else a new key)
	MnemonicTo  string // where a new mnemonic goes on the key machine (0600)
	AllowOnline bool
}

var (
	nodeIDRe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	monikerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// PrepareJoin makes a validator's keys on the machine c reaches: node and
// consensus keys from init, the operator key in the node's own keyring
// (new, or recovered from a mnemonic file there). Nothing secret comes
// back.
func PrepareJoin(c *toolkit.Context, spec *netspec.Spec, o JoinOpts) (*Join, string, error) {
	if !monikerRe.MatchString(o.Moniker) {
		return nil, "", fmt.Errorf("moniker %q: letters, digits, . _ - (up to 64)", o.Moniker)
	}
	h, err := c.Host()
	if err != nil {
		return nil, "", err
	}
	off, err := offlineGate(c, h, o.AllowOnline)
	if err != nil {
		return nil, "", err
	}
	image := ImageFor(spec)
	if _, code, _ := run(c, h, "docker image inspect "+common.ShellQ(image)+" >/dev/null 2>&1", time.Minute); code != 0 {
		return nil, "", fmt.Errorf("image %s isn't on this machine — load it while still online: cometcli host prepare --offline-ready --image %s", image, image)
	}
	abs, user, hasKey, err := prepareHome(c, h, o.Home)
	if err != nil {
		return nil, "", err
	}
	if hasKey {
		return nil, "", fmt.Errorf("%s already holds a node (config/priv_validator_key.json) — use a fresh home", abs)
	}
	ev := evmdOffline(user, abs, image)
	// a join that fails part-way removes the home it made (it held no node
	// before), so it can simply be run again
	done := false
	defer func() {
		if !done {
			_, _, _ = run(c, h, "rm -rf "+common.ShellQ(abs), time.Minute)
		}
	}()
	if out, code, err := run(c, h, fmt.Sprintf("%s init %s --chain-id %s --home %s >/dev/null 2>&1 && echo ok", ev, common.ShellQ(o.Moniker), common.ShellQ(spec.Chain.ID), containerHome), 3*time.Minute); err != nil || code != 0 || !strings.Contains(out, "ok") {
		return nil, "", fmt.Errorf("init failed: %v %s", err, out)
	}
	j := &Join{Version: FormatVersion, ChainID: spec.Chain.ID, Moniker: o.Moniker, IP: o.IP, P2PPort: o.P2PPort, Offline: off}
	if j.P2PPort == 0 {
		j.P2PPort = 26656
	}

	// the operator key: the mnemonic stays in a file on this machine — the
	// script returns only the address
	mnemonicTo := o.MnemonicTo
	if mnemonicTo == "" {
		mnemonicTo = abs + ".operator.mnemonic"
	}
	var script string
	if o.RecoverFrom != "" {
		script = fmt.Sprintf(`set -e; src=%[1]s; case "$src" in "~/"*) src="$HOME${src#\~}";; esac
test -f "$src" || { echo "no mnemonic file $src" >&2; exit 3; }
umask 077; tr -s ' \t\r\n' '   ' < "$src" | sed 's/^ //; s/ $//' | tr -d '\n' > %[2]s/.import.mnemonic
trap 'rm -f %[2]s/.import.mnemonic' EXIT
out=$(%[3]s keys add %[4]s --recover --source %[5]s/.import.mnemonic --keyring-backend %[6]s --home %[5]s --output json 2>%[2]s/.import.err) || { grep -v -i -E 'mnemonic|passphrase' %[2]s/.import.err | tail -3 >&2; rm -f %[2]s/.import.err; exit 4; }
rm -f %[2]s/.import.err; echo "$out" | sed -n 's/.*"address":"\([^"]*\)".*/\1/p'`,
			shellHome(o.RecoverFrom), common.ShellQ(abs), ev, common.ShellQ(o.Moniker), containerHome, o.Keyring.Backend)
	} else {
		script = fmt.Sprintf(`set -e; umask 077; out=%[1]s/.newkey.json; trap 'rm -f "$out"' EXIT
%[2]s keys add %[3]s --keyring-backend %[4]s --home %[5]s --output json > "$out" 2>/dev/null
m=%[6]s; case "$m" in "~/"*) m="$HOME${m#\~}";; esac
sed -n 's/.*"mnemonic":"\([^"]*\)".*/\1/p' "$out" > "$m"; chmod 600 "$m"; test -s "$m"
sed -n 's/.*"address":"\([^"]*\)".*/\1/p' "$out"`,
			common.ShellQ(abs), ev, common.ShellQ(o.Moniker), o.Keyring.Backend, containerHome, shellHome(mnemonicTo))
	}
	res, err := host.ExecIn(c, h, script, o.Keyring.stdin(2), 1<<16)
	if err != nil || res.Code != 0 {
		return nil, "", fmt.Errorf("creating the operator key failed (exit %d): %s", res.Code, strings.TrimSpace(res.Output))
	}
	j.Account = strings.TrimSpace(lastLine(res.Output))
	if !strings.HasPrefix(j.Account, spec.Bech32()+"1") {
		return nil, "", fmt.Errorf("unexpected operator address %q", j.Account)
	}
	id, _, _ := run(c, h, ev+" comet show-node-id --home "+containerHome+" 2>/dev/null | tail -1", time.Minute)
	j.NodeID = strings.TrimSpace(id)
	pk, _, _ := run(c, h, ev+" comet show-validator --home "+containerHome+" 2>/dev/null | tail -1", time.Minute)
	var cons struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(pk)), &cons)
	j.ConsKey = cons.Key
	if !nodeIDRe.MatchString(j.NodeID) || j.ConsKey == "" {
		return nil, "", fmt.Errorf("reading the node id and consensus key failed")
	}
	where := mnemonicTo
	if o.RecoverFrom != "" {
		where = ""
	}
	done = true
	return j, where, nil
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return l[len(l)-1]
}

// ReadJoin reads and checks a join file.
func ReadJoin(path string, spec *netspec.Spec) (*Join, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j Join
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case j.Version != FormatVersion:
		return nil, fmt.Errorf("%s: format version %d, want %d", path, j.Version, FormatVersion)
	case j.ChainID != spec.Chain.ID:
		return nil, fmt.Errorf("%s: chain %q, not %s", path, j.ChainID, spec.Chain.ID)
	case !monikerRe.MatchString(j.Moniker):
		return nil, fmt.Errorf("%s: bad moniker %q", path, j.Moniker)
	case !strings.HasPrefix(j.Account, spec.Bech32()+"1"):
		return nil, fmt.Errorf("%s: account %q isn't a %s address", path, j.Account, spec.Bech32())
	case !nodeIDRe.MatchString(j.NodeID):
		return nil, fmt.Errorf("%s: bad node id %q", path, j.NodeID)
	case j.ConsKey == "":
		return nil, fmt.Errorf("%s: no consensus pubkey", path)
	}
	if _, err := keys.ValAddress(j.Account); err != nil {
		return nil, fmt.Errorf("%s: account %q: %w", path, j.Account, err)
	}
	return &j, nil
}

// --- kit -------------------------------------------------------------------

// Kit is what the coordinator hands every validator after the base
// genesis: the base genesis, the spec, and who's in it.
type Kit struct {
	Version    int            `json:"version"`
	ChainID    string         `json:"chain_id"`
	Denom      string         `json:"denom"`
	Stake      string         `json:"stake"`
	Image      string         `json:"image"`
	Validators []KitValidator `json:"validators"`
	Created    string         `json:"created"`
}

// KitValidator is a genesis validator as everyone sees it.
type KitValidator struct {
	Moniker string `json:"moniker"`
	Account string `json:"account"`
	NodeID  string `json:"node_id"`
	ConsKey string `json:"consensus_pubkey,omitempty"`
	Address string `json:"address"` // ip:port peers dial
	Offline bool   `json:"keys_made_offline"`
}

// writeManifest writes SHA256SUMS over files in dir and returns its hash
// (the id the next step checks).
func writeManifest(dir string, files []string) (string, error) {
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(raw)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), f)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	id := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(id[:]), nil
}

// verifyManifest checks every file SHA256SUMS lists and returns its hash.
func verifyManifest(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return "", fmt.Errorf("%s has no SHA256SUMS — not a cometcli kit or package", dir)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || strings.ContainsAny(f[1], `/\`) {
			return "", fmt.Errorf("SHA256SUMS: bad line %q", l)
		}
		b, err := os.ReadFile(filepath.Join(dir, f[1]))
		if err != nil {
			return "", fmt.Errorf("%s is missing", f[1])
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f[0] {
			return "", fmt.Errorf("%s doesn't match its checksum — the kit was changed or damaged in transit", f[1])
		}
	}
	id := sha256.Sum256(raw)
	return hex.EncodeToString(id[:]), nil
}

const kitReadme = `Genesis kit for %[1]s

Check it first: every file is listed with its sha256 in SHA256SUMS, and
cometcli verifies them before using anything.

On your validator machine — offline if you can (the image must be loaded:
cometcli host prepare --offline-ready, while still online):

  cometcli genesis gentx --kit <this dir> --home <your node home>

Send back the one file it writes (<moniker>.gentx.json). It holds your
signed gentx and public node info — nothing secret. Keep your node home's
config/priv_validator_key.json and your operator mnemonic backed up off
the machine, and never run the same key on two machines (double-sign).
`

// WriteKit writes the kit to dir and returns its id.
func WriteKit(dir string, k *Kit, base []byte, specPath string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	spec, err := os.ReadFile(specPath)
	if err != nil {
		return "", err
	}
	kj, _ := json.MarshalIndent(k, "", "  ")
	files := map[string][]byte{
		"base-genesis.json": base, "spec.yaml": spec, "kit.json": kj,
		"README.txt": []byte(fmt.Sprintf(kitReadme, k.ChainID)),
	}
	var names []string
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o644); err != nil {
			return "", err
		}
		names = append(names, n)
	}
	return writeManifest(dir, names)
}

// ReadKit verifies a kit and returns it with its base genesis, its spec
// and its id.
func ReadKit(dir string) (*Kit, []byte, *netspec.Spec, string, error) {
	id, err := verifyManifest(dir)
	if err != nil {
		return nil, nil, nil, "", err
	}
	var k Kit
	raw, _ := os.ReadFile(filepath.Join(dir, "kit.json"))
	if err := json.Unmarshal(raw, &k); err != nil || k.Version != FormatVersion {
		return nil, nil, nil, "", fmt.Errorf("kit.json: unreadable or format version %d", k.Version)
	}
	base, _ := os.ReadFile(filepath.Join(dir, "base-genesis.json"))
	spec, _, err := netspec.Load(filepath.Join(dir, "spec.yaml"))
	if err != nil {
		return nil, nil, nil, "", err
	}
	if spec.Chain.ID != k.ChainID {
		return nil, nil, nil, "", fmt.Errorf("the kit's spec is for %s, the kit for %s", spec.Chain.ID, k.ChainID)
	}
	var g struct {
		ChainID string `json:"chain_id"`
	}
	if json.Unmarshal(base, &g) != nil || g.ChainID != k.ChainID {
		return nil, nil, nil, "", fmt.Errorf("the base genesis isn't for %s", k.ChainID)
	}
	return &k, base, spec, id, nil
}

// --- gentx -----------------------------------------------------------------

// GentxReturn is the one file a validator sends back.
type GentxReturn struct {
	Version int             `json:"version"`
	ChainID string          `json:"chain_id"`
	KitID   string          `json:"kit_id"`
	Moniker string          `json:"moniker"`
	Account string          `json:"account"`
	NodeID  string          `json:"node_id"`
	Gentx   json.RawMessage `json:"gentx"`
	Offline Offline         `json:"offline"`
}

// SignGentx signs this machine's gentx against the kit's base genesis, in
// a container with no network, after checking the node home is the one
// the kit lists.
func SignGentx(c *toolkit.Context, k *Kit, kitID string, base []byte, spec *netspec.Spec, homeDir, moniker string, kr KeyringOpts, allowOnline bool) (*GentxReturn, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	off, err := offlineGate(c, h, allowOnline)
	if err != nil {
		return nil, err
	}
	var me *KitValidator
	for i := range k.Validators {
		if k.Validators[i].Moniker == moniker {
			me = &k.Validators[i]
		}
	}
	if me == nil {
		return nil, fmt.Errorf("%s isn't a validator in this kit", moniker)
	}
	abs, user, hasKey, err := prepareHome(c, h, homeDir)
	if err != nil {
		return nil, err
	}
	if !hasKey {
		return nil, fmt.Errorf("%s has no node keys — this must be the home genesis join made", abs)
	}
	ev := evmdOffline(user, abs, k.Image)
	id, _, _ := run(c, h, ev+" comet show-node-id --home "+containerHome+" 2>/dev/null | tail -1", time.Minute)
	if strings.TrimSpace(id) != me.NodeID {
		return nil, fmt.Errorf("this home's node id %s isn't %s's in the kit (%s) — wrong home?", strings.TrimSpace(id), moniker, me.NodeID)
	}
	n := &GenesisNode{Ctx: c, Home: abs, User: user, Moniker: moniker, Account: me.Account, NodeID: me.NodeID, Image: k.Image}
	g, err := gentxIn(n, ev, spec, base, kr, me.Address)
	if err != nil {
		return nil, err
	}
	return &GentxReturn{Version: FormatVersion, ChainID: k.ChainID, KitID: kitID, Moniker: moniker, Account: me.Account, NodeID: me.NodeID, Gentx: json.RawMessage(strings.TrimSpace(string(g))), Offline: off}, nil
}

// --- collect ---------------------------------------------------------------

// gentxDoc is the part of a gentx collect checks.
type gentxDoc struct {
	Body struct {
		Messages []struct {
			Type        string `json:"@type"`
			Description struct {
				Moniker string `json:"moniker"`
			} `json:"description"`
			Delegator string `json:"delegator_address"`
			Validator string `json:"validator_address"`
			Pubkey    struct {
				Key string `json:"key"`
			} `json:"pubkey"`
			Value struct {
				Denom  string `json:"denom"`
				Amount string `json:"amount"`
			} `json:"value"`
		} `json:"messages"`
		Memo string `json:"memo"`
	} `json:"body"`
	Signatures []string `json:"signatures"`
}

// CheckReturn verifies a returned gentx against the kit before anything
// is built from it. The signature itself is checked by validate-genesis.
func CheckReturn(k *Kit, kitID string, r *GentxReturn) error {
	if r.Version != FormatVersion {
		return fmt.Errorf("%s: format version %d", r.Moniker, r.Version)
	}
	if r.ChainID != k.ChainID || r.KitID != kitID {
		return fmt.Errorf("%s: signed against a different kit (chain %s, kit %.12s…, this kit %.12s…) — send them the current kit", r.Moniker, r.ChainID, r.KitID, kitID)
	}
	var me *KitValidator
	for i := range k.Validators {
		if k.Validators[i].Moniker == r.Moniker {
			me = &k.Validators[i]
		}
	}
	if me == nil {
		return fmt.Errorf("%s isn't in the kit", r.Moniker)
	}
	var g gentxDoc
	if err := json.Unmarshal(r.Gentx, &g); err != nil || len(g.Body.Messages) != 1 {
		return fmt.Errorf("%s: the gentx isn't one create-validator message", r.Moniker)
	}
	m := g.Body.Messages[0]
	valoper, _ := keys.ValAddress(me.Account)
	switch {
	case m.Type != "/cosmos.staking.v1beta1.MsgCreateValidator":
		return fmt.Errorf("%s: gentx message is %s", r.Moniker, m.Type)
	case m.Description.Moniker != r.Moniker:
		return fmt.Errorf("%s: gentx moniker is %q", r.Moniker, m.Description.Moniker)
	case m.Validator != valoper || (m.Delegator != "" && m.Delegator != me.Account):
		return fmt.Errorf("%s: gentx is for %s, the kit funds %s", r.Moniker, m.Validator, valoper)
	case m.Value.Denom != k.Denom || m.Value.Amount != k.Stake:
		return fmt.Errorf("%s: stakes %s%s, the agreed amount is %s%s", r.Moniker, m.Value.Amount, m.Value.Denom, k.Stake, k.Denom)
	case me.ConsKey != "" && m.Pubkey.Key != me.ConsKey:
		return fmt.Errorf("%s: gentx consensus key isn't the one in the join file — a different home signed it", r.Moniker)
	case !strings.HasPrefix(g.Body.Memo, me.NodeID+"@"):
		return fmt.Errorf("%s: gentx memo %q isn't this node (%s)", r.Moniker, g.Body.Memo, me.NodeID)
	case len(g.Signatures) != 1 || g.Signatures[0] == "":
		return fmt.Errorf("%s: gentx isn't signed", r.Moniker)
	}
	return nil
}

// CollectAt assembles and strictly validates the final genesis on the
// builder's host from the kit's base genesis and the gentxs.
func CollectAt(c *toolkit.Context, spec *netspec.Spec, base []byte, gentxs map[string][]byte) ([]byte, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	dir, _, _, err := prepareHome(c, h, "~/.cometcli-build/genesis-"+spec.Chain.ID)
	if err != nil {
		return nil, err
	}
	if err := h.WriteFile(c, path.Join(dir, "config", "genesis.json"), base, 0o644); err != nil {
		return nil, fmt.Errorf("the builder home on %s has no config dir — run genesis base first: %w", c.Profile.Name, err)
	}
	return Collect(c, spec, gentxs)
}

// --- packages --------------------------------------------------------------

// Package is what a validator installs: the final genesis and its peers.
type Package struct {
	Version int      `json:"version"`
	ChainID string   `json:"chain_id"`
	Moniker string   `json:"moniker"`
	NodeID  string   `json:"node_id"`
	Genesis string   `json:"genesis_sha256"`
	Peers   []string `json:"peers"`
}

// WritePackages writes one package per validator under dir: the final
// genesis, the spec, and every other validator as a peer.
func WritePackages(dir string, k *Kit, final []byte, specPath string) (map[string]string, error) {
	sum := sha256.Sum256(final)
	spec, err := os.ReadFile(specPath)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, v := range k.Validators {
		var peers []string
		for _, o := range k.Validators {
			if o.Moniker != v.Moniker && o.Address != "" {
				peers = append(peers, o.NodeID+"@"+o.Address)
			}
		}
		pd := filepath.Join(dir, v.Moniker)
		if err := os.MkdirAll(pd, 0o755); err != nil {
			return nil, err
		}
		pk := Package{Version: FormatVersion, ChainID: k.ChainID, Moniker: v.Moniker, NodeID: v.NodeID, Genesis: hex.EncodeToString(sum[:]), Peers: peers}
		pj, _ := json.MarshalIndent(pk, "", "  ")
		for n, b := range map[string][]byte{"genesis.json": final, "spec.yaml": spec, "package.json": pj} {
			if err := os.WriteFile(filepath.Join(pd, n), b, 0o644); err != nil {
				return nil, err
			}
		}
		if _, err := writeManifest(pd, []string{"genesis.json", "spec.yaml", "package.json"}); err != nil {
			return nil, err
		}
		out[v.Moniker] = pd
	}
	return out, nil
}

// ReadPackage verifies a package.
func ReadPackage(dir string) (*Package, []byte, string, error) {
	if _, err := verifyManifest(dir); err != nil {
		return nil, nil, "", err
	}
	var p Package
	raw, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if err := json.Unmarshal(raw, &p); err != nil || p.Version != FormatVersion {
		return nil, nil, "", fmt.Errorf("package.json: unreadable or format version %d", p.Version)
	}
	gen, _ := os.ReadFile(filepath.Join(dir, "genesis.json"))
	sum := sha256.Sum256(gen)
	if hex.EncodeToString(sum[:]) != p.Genesis {
		return nil, nil, "", fmt.Errorf("genesis.json isn't the one the package names")
	}
	return &p, gen, filepath.Join(dir, "spec.yaml"), nil
}

// InstallPackage puts the final genesis into the validator's home (made
// by genesis join) and starts the node with the package's peers.
func InstallPackage(c *toolkit.Context, p *Package, gen []byte, specPath, homeDir string, portOffset int, extIP string) (*toolkit.Result, error) {
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	abs, user, hasKey, err := prepareHome(c, h, homeDir)
	if err != nil {
		return nil, err
	}
	if !hasKey {
		return nil, fmt.Errorf("%s has no node keys — install into the home genesis join made", abs)
	}
	spec, _, err := netspec.Load(specPath)
	if err != nil {
		return nil, err
	}
	id, _, _ := run(c, h, evmdOffline(user, abs, ImageFor(spec))+" comet show-node-id --home "+containerHome+" 2>/dev/null | tail -1", time.Minute)
	if strings.TrimSpace(id) != p.NodeID {
		return nil, fmt.Errorf("this home is node %s, the package is for %s (%s) — wrong home or wrong package", strings.TrimSpace(id), p.Moniker, p.NodeID)
	}
	if err := h.WriteFile(c, path.Join(abs, "config", "genesis.json"), gen, 0o644); err != nil {
		return nil, err
	}
	args := toolkit.Args{"role": "validator", "network": specPath, "home": abs, "moniker": p.Moniker, "reconfigure": true,
		"peers": strings.Join(p.Peers, ","), "port_offset": float64(portOffset)}
	if extIP != "" {
		args["external_ip"] = extIP
	}
	return (provisionTool{}).Run(c, args)
}

// --- removal ---------------------------------------------------------------

// RemoveNode stops a node and deletes its home — only when the home's
// genesis is the named chain's, so another network's data is never
// touched. It returns how much space was freed.
func RemoveNode(c *toolkit.Context, chainID, homeDir, unit string) (string, error) {
	h, err := c.Host()
	if err != nil {
		return "", err
	}
	out, _, _ := run(c, h, fmt.Sprintf(`h=%s; case "$h" in "~"|"~/"*) h="$HOME${h#\~}";; esac; test -d "$h" || { echo none; exit 0; }
c=$(grep -m1 -o '"chain_id": *"[^"]*"' "$h/config/genesis.json" 2>/dev/null | sed 's/.*"\([^"]*\)"$/\1/'); echo "$h ${c:--} $(du -sh "$h" 2>/dev/null | cut -f1)"`, shellHome(homeDir)), time.Minute)
	f := strings.Fields(out)
	if len(f) == 1 && f[0] == "none" {
		return "nothing at " + homeDir, nil
	}
	if len(f) < 3 {
		return "", fmt.Errorf("reading %s: %s", homeDir, out)
	}
	abs, chain, size := f[0], f[1], f[2]
	if chain != chainID {
		return "", fmt.Errorf("%s holds chain %q, not %s — not removed", abs, chain, chainID)
	}
	if abs == "/" || strings.Count(strings.Trim(abs, "/"), "/") < 1 {
		return "", fmt.Errorf("refusing to remove %s", abs)
	}
	script := fmt.Sprintf(`cd %[1]s 2>/dev/null && { docker compose -f docker-compose.yml down -t 20 >/dev/null 2>&1 || true; }
docker rm -f %[2]s >/dev/null 2>&1 || true
rm -rf %[1]s %[1]s.operator.mnemonic && echo removed`, common.ShellQ(abs), common.ShellQ(unit))
	if out, _, err := run(c, h, script, 3*time.Minute); err != nil || !strings.Contains(out, "removed") {
		return "", fmt.Errorf("removing %s: %v %s", abs, err, out)
	}
	return fmt.Sprintf("removed %s (%s, chain %s)", abs, size, chain), nil
}

// RemoveBuilder deletes the chain's genesis builder home on a host.
func RemoveBuilder(c *toolkit.Context, chainID string) string {
	h, err := c.Host()
	if err != nil {
		return ""
	}
	dir := "~/.cometcli-build/genesis-" + chainID
	out, _, _ := run(c, h, fmt.Sprintf(`d="$HOME/.cometcli-build/genesis-%s"; test -d "$d" && { s=$(du -sh "$d" | cut -f1); rm -rf "$d" && echo "removed %s ($s)"; } || true`, chainID, dir), time.Minute)
	return strings.TrimSpace(out)
}
