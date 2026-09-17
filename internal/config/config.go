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
	Active   string              `yaml:"active"`
	Profiles map[string]*Profile `yaml:"profiles"`
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
}

// IsValidator reports whether this profile is a consensus-signing validator.
func (p *Profile) IsValidator() bool { return p.Role == "validator" }

// Endpoints are the network surfaces of the node. Empty means "not exposed".
type Endpoints struct {
	Comet string `yaml:"comet,omitempty"` // CometBFT RPC, e.g. tcp://127.0.0.1:26657 or http://
	GRPC  string `yaml:"grpc,omitempty"`  // host:port
	LCD   string `yaml:"lcd,omitempty"`   // http(s)://host:1317
	EVM   string `yaml:"evm,omitempty"`   // http(s)://host:8545 (RPC/sentry nodes only)
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
}

// AgentConf configures the LLM backend for agent mode.
type AgentConf struct {
	Provider  string `yaml:"provider"` // anthropic | openai | openai-compat | off
	Model     string `yaml:"model,omitempty"`
	BaseURL   string `yaml:"base_url,omitempty"`
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
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
