package cli

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/config"
)

// agentKeys are the settable global agent keys and their setters.
var agentKeys = map[string]func(*config.AgentConf, string) error{
	"provider":    func(a *config.AgentConf, v string) error { a.Provider = v; return nil },
	"model":       func(a *config.AgentConf, v string) error { a.Model = v; return nil },
	"base_url":    func(a *config.AgentConf, v string) error { a.BaseURL = v; return nil },
	"api_key_env": func(a *config.AgentConf, v string) error { a.APIKeyEnv = v; return nil },
	"effort": func(a *config.AgentConf, v string) error {
		if err := agent.ValidEffort(v); err != nil {
			return err
		}
		a.Effort = v
		return nil
	},
	"mode": func(a *config.AgentConf, v string) error {
		if _, err := agent.ParseMode(v); err != nil {
			return err
		}
		a.Mode = v
		return nil
	},
	"tools": func(a *config.AgentConf, v string) error {
		if v != "" && v != "all" && v != "auto" {
			return fmt.Errorf("tools: want all or auto")
		}
		a.Tools = v
		return nil
	},
	"max_tokens":     intKey(func(a *config.AgentConf) *int { return &a.MaxTokens }),
	"max_turns":      intKey(func(a *config.AgentConf) *int { return &a.MaxTurns }),
	"context_window": intKey(func(a *config.AgentConf) *int { return &a.ContextWindow }),
	"compact_at":     intKey(func(a *config.AgentConf) *int { return &a.CompactAt }),
	"no_stream": func(a *config.AgentConf, v string) error {
		b, err := strconv.ParseBool(v)
		a.NoStream = b
		return err
	},
}

func intKey(f func(*config.AgentConf) *int) func(*config.AgentConf, string) error {
	return func(a *config.AgentConf, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("want a number, got %q", v)
		}
		*f(a) = n
		return nil
	}
}

// configCmd reads and writes the global agent settings in config.yaml —
// the defaults general mode uses and profiles override.
func configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Show or change global agent settings (agent.provider, agent.model, …)"}
	keys := make([]string, 0, len(agentKeys))
	for k := range agentKeys {
		keys = append(keys, "agent."+k)
	}
	sort.Strings(keys)
	cmd.AddCommand(&cobra.Command{
		Use:   "set-key <ENV_NAME>",
		Short: "Save an API key (read from stdin) to ~/.cometcli/credentials (0600), e.g. set-key OPENROUTER_API_KEY",
		Long: "Reads the key from stdin (hidden when it's a terminal) and saves it to ~/.cometcli/credentials, mode 0600. " +
			"Providers use it when the environment variable isn't set. An empty input removes the key.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := readSecretStdin(cmd, args[0])
			if err != nil {
				return err
			}
			if err := config.SetCredential(args[0], strings.TrimSpace(v)); err != nil {
				return err
			}
			p, _ := config.Path("credentials")
			if strings.TrimSpace(v) == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s\n", args[0], p)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "saved %s to %s (0600)\n", args[0], p)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the global agent section",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			b, _ := yaml.Marshal(map[string]any{"agent": cfg.Agent})
			fmt.Fprint(cmd.OutOrStdout(), string(b))
			if cfg.Agent.Provider == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "# no global provider — general mode uses the active profile's agent settings")
			}
			return nil
		},
	}, &cobra.Command{
		Use:       "set <key> <value>",
		Short:     "Set a key: " + strings.Join(keys, ", "),
		Args:      cobra.ExactArgs(2),
		ValidArgs: keys,
		RunE: func(cmd *cobra.Command, args []string) error {
			k := strings.TrimPrefix(args[0], "agent.")
			set, ok := agentKeys[k]
			if !ok {
				return fmt.Errorf("unknown key %q — one of %s", args[0], strings.Join(keys, ", "))
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if err := set(&cfg.Agent, args[1]); err != nil {
				return err
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "agent.%s = %s\n", k, args[1])
			return nil
		},
	}, &cobra.Command{
		Use:   "unset <key>",
		Short: "Clear a key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k := strings.TrimPrefix(args[0], "agent.")
			set, ok := agentKeys[k]
			if !ok {
				return fmt.Errorf("unknown key %q", args[0])
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			zero := ""
			if strings.HasPrefix(k, "max_") || k == "context_window" || k == "compact_at" {
				zero = "0"
			} else if k == "no_stream" {
				zero = "false"
			}
			if err := set(&cfg.Agent, zero); err != nil {
				return err
			}
			return cfg.Save()
		},
	})
	return cmd
}

// readSecretStdin reads one line from stdin, without echo on a terminal.
func readSecretStdin(cmd *cobra.Command, name string) (string, error) {
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: ", name)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(cmd.ErrOrStderr())
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no key on stdin")
	}
	return line, nil
}
