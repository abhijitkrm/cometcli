// Package config manages cometcli profiles and the on-disk configuration
// file (~/.cometcli/config.yaml). Secrets are never stored here — they live
// in the OS keychain or are referenced via env: indirection.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Dir returns the cometcli home directory, creating it if needed.
func Dir() (string, error) {
	if d := os.Getenv("COMETCLI_HOME"); d != "" {
		return d, os.MkdirAll(d, 0o700)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, ".cometcli")
	return d, os.MkdirAll(d, 0o700)
}

// Path returns the path to a file inside the cometcli home dir.
func Path(elem ...string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{d}, elem...)...), nil
}

const configFile = "config.yaml"

// Config is the root of ~/.cometcli/config.yaml.
type Config struct {
	Active string `yaml:"active"`
	// Agent is the default LLM setup, used on its own in general mode
	// (no node) and as the base a profile's agent section overrides.
	Agent    AgentConf           `yaml:"agent,omitempty"`
	Profiles map[string]*Profile `yaml:"profiles"`
	// Watch configures `cometcli watch`, the autonomous fleet watcher.
	Watch WatchConf `yaml:"watch,omitempty"`
}

// WatchConf configures the autonomous watcher.
type WatchConf struct {
	Interval string   `yaml:"interval,omitempty"` // between sweeps (default 5m)
	Mode     string   `yaml:"mode,omitempty"`     // notify | diagnose (default) | fix
	Profiles []string `yaml:"profiles,omitempty"` // default: all
	Cooldown string   `yaml:"cooldown,omitempty"` // before the same issue is worked again (default 1h)
	// ApprovalTimeout is how long a remote approval waits before it's a
	// "no" (default 15m).
	ApprovalTimeout string `yaml:"approval_timeout,omitempty"`
	// AllowTx lets fix mode request transaction approvals remotely;
	// without it, transactions are always refused in watch mode.
	AllowTx bool `yaml:"allow_tx,omitempty"`
	// Alerts are where findings, reports and approvals go. Telegram is
	// the approval channel (token: here or the TELEGRAM_BOT_TOKEN
	// credential); Slack and Discord are notify-only.
	Alerts Alerts `yaml:"alerts,omitempty"`
	// TelegramUsers, when set, are the only Telegram user ids whose
	// button presses count.
	TelegramUsers []int64 `yaml:"telegram_users,omitempty"`
}

// AgentFor resolves the agent settings for a profile (nil = general
// mode): the global agent section, overridden field by field by the
// profile's. In general mode without a global provider, the active
// profile's settings are used, so existing setups keep working.
func (c *Config) AgentFor(p *Profile) AgentConf {
	if c == nil {
		if p != nil {
			return p.Agent
		}
		return AgentConf{}
	}
	if p == nil {
		if c.Agent.Provider != "" {
			return c.Agent
		}
		if ap, err := c.ActiveProfile(""); err == nil && ap != nil {
			return MergeAgent(c.Agent, ap.Agent)
		}
		return c.Agent
	}
	return MergeAgent(c.Agent, p.Agent)
}

// MergeAgent overlays o's set fields onto base. A different provider in o
// drops base's model, endpoint and key (they belong to the old provider);
// permission rules accumulate.
func MergeAgent(base, o AgentConf) AgentConf {
	r := base
	if o.Provider != "" && o.Provider != base.Provider {
		r.Provider, r.Model, r.BaseURL, r.APIKeyEnv = o.Provider, "", "", ""
	}
	str := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	num := func(dst *int, v int) {
		if v != 0 {
			*dst = v
		}
	}
	str(&r.Model, o.Model)
	str(&r.BaseURL, o.BaseURL)
	str(&r.APIKeyEnv, o.APIKeyEnv)
	str(&r.Mode, o.Mode)
	str(&r.Effort, o.Effort)
	str(&r.Tools, o.Tools)
	num(&r.MaxTokens, o.MaxTokens)
	num(&r.MaxTurns, o.MaxTurns)
	num(&r.ContextWindow, o.ContextWindow)
	num(&r.CompactAt, o.CompactAt)
	if len(o.Autopilot) > 0 {
		r.Autopilot = o.Autopilot
	}
	r.RedactHosts = append(append([]string{}, base.RedactHosts...), o.RedactHosts...)
	r.RedactEndpoints = base.RedactEndpoints || o.RedactEndpoints
	r.NoStream = base.NoStream || o.NoStream
	r.Permissions = Permissions{
		Allow: append(append([]string{}, base.Permissions.Allow...), o.Permissions.Allow...),
		Ask:   append(append([]string{}, base.Permissions.Ask...), o.Permissions.Ask...),
		Deny:  append(append([]string{}, base.Permissions.Deny...), o.Permissions.Deny...),
	}
	return r
}

// Profile describes one managed node.
type Profile struct {
	Name         string            `yaml:"-"`
	ChainID      string            `yaml:"chain_id"`
	EVMChainID   uint64            `yaml:"evm_chain_id,omitempty"`
	Bech32Prefix string            `yaml:"bech32_prefix,omitempty"`
	Role         string            `yaml:"role"`   // validator | rpc | sentry
	Home         string            `yaml:"home"`   // node home dir, e.g. ~/.evmd
	Binary       string            `yaml:"binary"` // chain binary name, e.g. evmd
	Endpoints    Endpoints         `yaml:"endpoints"`
	Transport    Transport         `yaml:"transport"`
	Service      Service           `yaml:"service"`
	Signer       Signer            `yaml:"signer"`
	Agent        AgentConf         `yaml:"agent"`
	Alerts       Alerts            `yaml:"alerts"`
	Metadata     map[string]string `yaml:"metadata,omitempty"`
	// Sentries names the profiles of the sentry nodes this validator
	// peers through (fleet triage checks it isn't cut off).
	Sentries []string `yaml:"sentries,omitempty"`
}

// IsValidator reports whether this profile is a consensus-signing validator.
func (p *Profile) IsValidator() bool { return p.Role == "validator" }

// Endpoints are the network surfaces of the node. Empty means "not exposed".
type Endpoints struct {
	Comet string `yaml:"comet,omitempty"` // CometBFT RPC, e.g. tcp://127.0.0.1:26657 or http://
	GRPC  string `yaml:"grpc,omitempty"`  // host:port
	LCD   string `yaml:"lcd,omitempty"`   // http(s)://host:1317
	EVM   string `yaml:"evm,omitempty"`   // http(s)://host:8545 (RPC/sentry nodes only)
	// FallbackGRPC is another node of the same chain (sentry, RPC node,
	// public endpoint) used for chain queries while this node is down.
	FallbackGRPC string `yaml:"fallback_grpc,omitempty"`
}

// Transport selects how host-plane operations reach the machine.
type Transport struct {
	Type    string `yaml:"type"` // local | ssh
	Host    string `yaml:"host,omitempty"`
	User    string `yaml:"user,omitempty"`
	Port    int    `yaml:"port,omitempty"`
	KeyFile string `yaml:"key_file,omitempty"`
}

// Service describes how the node process is supervised.
type Service struct {
	Type string `yaml:"type"` // systemd | docker | launchd | none
	Unit string `yaml:"unit"` // unit name or container name
}

// Signer configures the ops keyring used for transaction signing.
// It NEVER refers to consensus key material.
type Signer struct {
	Backend  string `yaml:"backend"` // os | file | test
	Key      string `yaml:"key"`     // key name in the keyring
	CoinType uint32 `yaml:"coin_type,omitempty"`
	Account  uint32 `yaml:"account,omitempty"`
	Index    uint32 `yaml:"index,omitempty"`
	// Mode picks the signer when both are available: "" asks at signing
	// time, "local" uses cometcli's keyring, "container" the node's.
	Mode string `yaml:"mode,omitempty"`
	// Container signing: the key lives in the node container's own
	// keyring and evmd signs there (`docker exec … evmd tx sign`).
	Container        string `yaml:"container,omitempty"`         // default: service.unit for docker services
	ContainerKey     string `yaml:"container_key,omitempty"`     // default: key
	ContainerKeyring string `yaml:"container_keyring,omitempty"` // test | file (default: detect)
	ContainerHome    string `yaml:"container_home,omitempty"`    // evmd --home inside the container
	ContainerBinary  string `yaml:"container_binary,omitempty"`  // default: the profile binary
}

// AgentConf configures the LLM backend for agent mode.
type AgentConf struct {
	Provider  string `yaml:"provider"` // anthropic | openai | openai-compat | off
	Model     string `yaml:"model,omitempty"`
	BaseURL   string `yaml:"base_url,omitempty"`
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
	// Mode is the default approval posture: ops (default) | readonly.
	Mode string `yaml:"mode,omitempty"`
	// Autopilot lists tiers that run without a confirm prompt. Only
	// local-change is accepted; on-chain can never be autopiloted.
	Autopilot []string `yaml:"autopilot,omitempty"`
	// RedactHosts are hostnames/IPs masked before any text reaches the LLM.
	RedactHosts []string `yaml:"redact_hosts,omitempty"`
	// RedactEndpoints also masks the profile's own endpoint/SSH hosts.
	RedactEndpoints bool `yaml:"redact_endpoints,omitempty"`
	// NoStream disables token streaming for endpoints that mishandle it.
	NoStream bool `yaml:"no_stream,omitempty"`
	// Effort is the reasoning depth: low | medium | high | xhigh | max
	// (empty = model default). Mapped to each provider's own control.
	Effort string `yaml:"effort,omitempty"`
	// MaxTokens caps output tokens per model round (0 = provider default).
	MaxTokens int `yaml:"max_tokens,omitempty"`
	// MaxTurns bounds model rounds per prompt (0 = 200).
	MaxTurns int `yaml:"max_turns,omitempty"`
	// ContextWindow overrides the model's context size in tokens.
	ContextWindow int `yaml:"context_window,omitempty"`
	// CompactAt is the prompt size in tokens that triggers summarizing
	// the conversation (0 = 80% of the window, at most 200k).
	CompactAt int `yaml:"compact_at,omitempty"`
	// Tools is "all" to send every tool schema on every request; the
	// default loads non-core tools on demand via tool_search.
	Tools string `yaml:"tools,omitempty"`
	// Permissions are allow/ask/deny rules, e.g. "bash(systemctl status:*)",
	// "edit(./config/**)", "web_fetch(domain:github.com)", "val.unjail".
	Permissions Permissions `yaml:"permissions,omitempty"`
}

// Permissions holds permission rule lists. Deny beats ask beats allow.
type Permissions struct {
	Allow []string `yaml:"allow,omitempty"`
	Ask   []string `yaml:"ask,omitempty"`
	Deny  []string `yaml:"deny,omitempty"`
}

// Alerts configures notification sinks for the monitor.
type Alerts struct {
	SlackWebhook   string `yaml:"slack_webhook,omitempty"`
	DiscordWebhook string `yaml:"discord_webhook,omitempty"`
	TelegramToken  string `yaml:"telegram_token,omitempty"`
	TelegramChatID string `yaml:"telegram_chat_id,omitempty"`
}

// Load reads the config file, returning an empty config if none exists.
func Load() (*Config, error) {
	p, err := Path(configFile)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Profiles: map[string]*Profile{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	for name, pr := range cfg.Profiles {
		pr.Name = name
	}
	return &cfg, nil
}

// Save writes the config atomically with 0600 permissions.
func (c *Config) Save() error {
	p, err := Path(configFile)
	if err != nil {
		return err
	}
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ActiveProfile returns the profile selected by `active` or the
// COMETCLI_PROFILE env var / --profile flag resolved by the caller.
func (c *Config) ActiveProfile(override string) (*Profile, error) {
	name := override
	if name == "" {
		name = os.Getenv("COMETCLI_PROFILE")
	}
	if name == "" {
		name = c.Active
	}
	if name == "" {
		return nil, errors.New("no active profile — run `cometcli profile add` or `cometcli profile use`")
	}
	p, ok := c.Profiles[name]
	if !ok {
		return nil, fmt.Errorf("profile %q not found", name)
	}
	p.Name = name
	return p, nil
}

// UpsertProfile inserts or replaces a named profile and saves.
func (c *Config) UpsertProfile(p *Profile) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("profile name required")
	}
	c.Profiles[p.Name] = p
	return c.Save()
}
