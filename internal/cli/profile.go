package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
)

func profileCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "profile", Short: "Manage node profiles"}
	cmd.AddCommand(
		&cobra.Command{Use: "list", Short: "List profiles", RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tROLE\tCHAIN-ID\tCOMET\tTRANSPORT\tACTIVE")
			for name, p := range cfg.Profiles {
				mark := ""
				if name == cfg.Active {
					mark = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", name, p.Role, p.ChainID, p.Endpoints.Comet, p.Transport.Type, mark)
			}
			return w.Flush()
		}},
		&cobra.Command{Use: "show <name>", Short: "Print a profile as YAML", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, a []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				p, ok := cfg.Profiles[a[0]]
				if !ok {
					return fmt.Errorf("profile %q not found", a[0])
				}
				b, _ := yaml.Marshal(p)
				fmt.Fprintf(cmd.OutOrStdout(), "# profile: %s\n%s", a[0], b)
				return nil
			}},
		&cobra.Command{Use: "use <name>", Short: "Set the active profile", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, a []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				if _, ok := cfg.Profiles[a[0]]; !ok {
					return fmt.Errorf("profile %q not found", a[0])
				}
				cfg.Active = a[0]
				return cfg.Save()
			}},
		profileAddCmd(),
		&cobra.Command{Use: "rm <name>", Short: "Delete a profile", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, a []string) error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				delete(cfg.Profiles, a[0])
				if cfg.Active == a[0] {
					cfg.Active = ""
				}
				return cfg.Save()
			}},
	)
	return cmd
}

func profileAddCmd() *cobra.Command {
	var p config.Profile
	var signerKey, feeDenom string
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add or update a node profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			// Merge onto the existing profile when present: only flags
			// actually passed override stored values.
			if existing, ok := cfg.Profiles[a[0]]; ok {
				np := *existing
				fl := cmd.Flags()
				if fl.Changed("chain-id") {
					np.ChainID = p.ChainID
				}
				if fl.Changed("evm-chain-id") {
					np.EVMChainID = p.EVMChainID
				}
				if fl.Changed("bech32-prefix") {
					np.Bech32Prefix = p.Bech32Prefix
				}
				if fl.Changed("role") {
					np.Role = p.Role
				}
				if fl.Changed("home") {
					np.Home = p.Home
				}
				if fl.Changed("binary") {
					np.Binary = p.Binary
				}
				if fl.Changed("comet") {
					np.Endpoints.Comet = p.Endpoints.Comet
				}
				if fl.Changed("grpc") {
					np.Endpoints.GRPC = p.Endpoints.GRPC
				}
				if fl.Changed("lcd") {
					np.Endpoints.LCD = p.Endpoints.LCD
				}
				if fl.Changed("evm") {
					np.Endpoints.EVM = p.Endpoints.EVM
				}
				if fl.Changed("transport") {
					np.Transport.Type = p.Transport.Type
				}
				if fl.Changed("ssh-host") {
					np.Transport.Host = p.Transport.Host
				}
				if fl.Changed("ssh-user") {
					np.Transport.User = p.Transport.User
				}
				if fl.Changed("ssh-port") {
					np.Transport.Port = p.Transport.Port
				}
				if fl.Changed("ssh-key") {
					np.Transport.KeyFile = p.Transport.KeyFile
				}
				if fl.Changed("service") {
					np.Service.Type = p.Service.Type
				}
				if fl.Changed("unit") {
					np.Service.Unit = p.Service.Unit
				}
				if fl.Changed("signer-backend") {
					np.Signer.Backend = p.Signer.Backend
				}
				if fl.Changed("agent-provider") {
					np.Agent.Provider = p.Agent.Provider
				}
				if fl.Changed("agent-model") {
					np.Agent.Model = p.Agent.Model
				}
				if fl.Changed("agent-base-url") {
					np.Agent.BaseURL = p.Agent.BaseURL
				}
				p = np
			}
			p.Name = a[0]
			if p.Role == "" {
				p.Role = "validator"
			}
			if p.Transport.Type == "" {
				p.Transport.Type = "local"
			}
			if signerKey != "" {
				p.Signer.Key = signerKey
			}
			if p.Signer.Backend == "" {
				p.Signer.Backend = "os"
			}
			if feeDenom != "" {
				if p.Metadata == nil {
					p.Metadata = map[string]string{}
				}
				p.Metadata["fee_denom"] = feeDenom
			}
			return cfg.UpsertProfile(&p)
		},
	}
	f := cmd.Flags()
	f.StringVar(&p.ChainID, "chain-id", "", "cosmos chain-id (e.g. mychain-1)")
	f.Uint64Var(&p.EVMChainID, "evm-chain-id", 0, "EIP-155 chain id")
	f.StringVar(&p.Bech32Prefix, "bech32-prefix", "", "bech32 prefix (e.g. cosmos)")
	f.StringVar(&p.Role, "role", "", "validator | rpc | sentry")
	f.StringVar(&p.Home, "home", "", "node home dir on the host (e.g. ~/.evmd)")
	f.StringVar(&p.Binary, "binary", "", "chain binary name (e.g. evmd)")
	f.StringVar(&p.Endpoints.Comet, "comet", "", "CometBFT RPC endpoint (tcp://host:26657)")
	f.StringVar(&p.Endpoints.GRPC, "grpc", "", "gRPC endpoint host:port")
	f.StringVar(&p.Endpoints.LCD, "lcd", "", "LCD/REST endpoint")
	f.StringVar(&p.Endpoints.EVM, "evm", "", "eth JSON-RPC endpoint (RPC/sentry nodes only)")
	f.StringVar(&p.Transport.Type, "transport", "", "local | ssh")
	f.StringVar(&p.Transport.Host, "ssh-host", "", "ssh host (transport=ssh)")
	f.StringVar(&p.Transport.User, "ssh-user", "", "ssh user")
	f.IntVar(&p.Transport.Port, "ssh-port", 22, "ssh port")
	f.StringVar(&p.Transport.KeyFile, "ssh-key", "", "ssh private key path")
	f.StringVar(&p.Service.Type, "service", "", "systemd | docker | launchd | none")
	f.StringVar(&p.Service.Unit, "unit", "", "service unit/container name")
	f.StringVar(&signerKey, "signer", "", "ops key name in keyring")
	f.StringVar(&p.Signer.Backend, "signer-backend", "", "os | file | test")
	f.StringVar(&feeDenom, "fee-denom", "", "fee denom (e.g. atest)")
	f.StringVar(&p.Agent.Provider, "agent-provider", "", "anthropic | openai | openai-compat | off")
	f.StringVar(&p.Agent.Model, "agent-model", "", "model name")
	f.StringVar(&p.Agent.BaseURL, "agent-base-url", "", "custom provider base URL")
	return cmd
}

func auditCmd() *cobra.Command {
	var tail int
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show audit log events (today by default)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			files, err := audit.List()
			if err != nil {
				return err
			}
			if len(files) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no audit events")
				return nil
			}
			raw, err := os.ReadFile(files[0])
			if err != nil {
				return err
			}
			lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
			if tail > 0 && len(lines) > tail {
				lines = lines[len(lines)-tail:]
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			for _, ln := range lines {
				var e audit.Event
				if json.Unmarshal([]byte(ln), &e) != nil {
					continue
				}
				d, _ := json.Marshal(e.Detail)
				if len(d) > 160 {
					d = append(d[:160], []byte("…")...)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					e.TS.Format("15:04:05"), e.Kind, e.Profile, d)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&tail, "tail", 30, "show last N events")
	return cmd
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "cometcli %s\n", Version)
		},
	}
}

// Version is stamped by goreleaser via ldflags.
var Version = "dev"
