package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/mcpclient"
	"github.com/abhijitkrm/cometcli/internal/settings"
)

// mcpScopePath returns the file a scope's servers live in: user settings,
// the project's .mcp.json (shared with Claude Code), or local settings.
func mcpScopePath(scope string) (string, error) {
	switch scope {
	case "user", "":
		return settings.UserPath()
	case "project", "local":
		cwd, _ := os.Getwd()
		root := settings.FindRoot(cwd)
		if root == "" {
			root = cwd
		}
		if scope == "project" {
			return filepath.Join(root, ".mcp.json"), nil
		}
		return filepath.Join(root, ".cometcli", "settings.local.json"), nil
	}
	return "", fmt.Errorf("--scope: want user, project or local")
}

// editServers mutates the mcpServers map of a scope's file.
func editServers(scope string, fn func(map[string]settings.MCPServer) error) (string, error) {
	p, err := mcpScopePath(scope)
	if err != nil {
		return "", err
	}
	if scope != "project" {
		return p, settings.EditFile(p, func(f *settings.File) error {
			if f.MCPServers == nil {
				f.MCPServers = map[string]settings.MCPServer{}
			}
			return fn(f.MCPServers)
		})
	}
	// .mcp.json: {"mcpServers": {...}}, other keys preserved
	doc := map[string]any{}
	if raw, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return p, fmt.Errorf("%s: %w", p, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return p, err
	}
	servers := map[string]settings.MCPServer{}
	if raw, ok := doc["mcpServers"]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &servers)
	}
	if err := fn(servers); err != nil {
		return p, err
	}
	doc["mcpServers"] = servers
	out, _ := json.MarshalIndent(doc, "", "  ")
	return p, os.WriteFile(p, append(out, '\n'), 0o644)
}

func mcpAddCmd() *cobra.Command {
	var scope, transport string
	var env, headers []string
	cmd := &cobra.Command{
		Use:   "add <name> [--transport http <url>] | [-- <command> [args…]]",
		Short: "Add an MCP server for the agent",
		Example: `  cometcli mcp add grafana -- npx -y @grafana/mcp-grafana
  cometcli mcp add k8s -e KUBECONFIG=~/.kube/config -- kubectl-mcp-server
  cometcli mcp add pagerduty --transport http https://mcp.example.com/mcp --header "Authorization: Bearer ${PD_TOKEN}"
  cometcli mcp add tools --scope project -- ./scripts/mcp.sh     # shared via .mcp.json`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			srv := settings.MCPServer{Type: transport}
			switch transport {
			case "http":
				srv.URL = args[1]
			case "stdio", "":
				srv.Type = ""
				srv.Command, srv.Args = args[1], args[2:]
			default:
				return fmt.Errorf("--transport: want stdio or http")
			}
			for _, kv := range env {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("-e %q: want KEY=value", kv)
				}
				if srv.Env == nil {
					srv.Env = map[string]string{}
				}
				srv.Env[k] = v
			}
			for _, h := range headers {
				k, v, ok := strings.Cut(h, ":")
				if !ok {
					return fmt.Errorf("--header %q: want 'Name: value'", h)
				}
				if srv.Headers == nil {
					srv.Headers = map[string]string{}
				}
				srv.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
			p, err := editServers(scope, func(m map[string]settings.MCPServer) error {
				m[name] = srv
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added MCP server %s to %s\n", name, p)
			if scope == "project" || scope == "local" {
				fmt.Fprintln(cmd.OutOrStdout(), "project servers run once the folder is trusted: cometcli trust")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&scope, "scope", "s", "user", "user | project (.mcp.json, shared) | local")
	cmd.Flags().StringVarP(&transport, "transport", "t", "stdio", "stdio | http")
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "KEY=value for a stdio server (${VAR} expands)")
	cmd.Flags().StringArrayVarP(&headers, "header", "H", nil, "'Name: value' for an http server (${VAR} expands)")
	return cmd
}

func mcpRemoveCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an MCP server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := editServers(scope, func(m map[string]settings.MCPServer) error {
				if _, ok := m[args[0]]; !ok {
					return fmt.Errorf("no MCP server %q in %s scope", args[0], scope)
				}
				delete(m, args[0])
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s\n", args[0], p)
			return nil
		},
	}
	cmd.Flags().StringVarP(&scope, "scope", "s", "user", "user | project | local")
	return cmd
}

func mcpListCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List MCP servers the agent will use here (--check connects to each)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, _ := os.Getwd()
			st, err := settings.Load(cwd)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(st.MCPServers))
			for n := range st.MCPServers {
				names = append(names, n)
			}
			sort.Strings(names)
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSCOPE\tTARGET\tSTATUS")
			for _, n := range names {
				srv := st.MCPServers[n]
				target := srv.URL
				if target == "" {
					target = strings.TrimSpace(srv.Command + " " + strings.Join(srv.Args, " "))
				}
				status := "-"
				if check {
					ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
					c, err := mcpclient.Connect(ctx, n, srv)
					cancel()
					if err != nil {
						status = "✗ " + err.Error()
					} else {
						status = fmt.Sprintf("✓ %d tools", len(c.Tools))
						c.Close()
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, srv.Scope, target, status)
			}
			if len(names) == 0 {
				fmt.Fprintln(tw, "(none)\t\t\t")
			}
			for _, ig := range st.Ignored {
				fmt.Fprintf(tw, "skipped\t\t%s\tproject not trusted — cometcli trust\n", ig)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "connect to each server and count its tools")
	return cmd
}

// trustCmd marks a project directory as trusted so its hooks and MCP
// servers may run.
func trustCmd() *cobra.Command {
	var off, list bool
	cmd := &cobra.Command{
		Use:   "trust [dir]",
		Short: "Trust a project folder: allow its hooks and MCP servers to run",
		Long: `Project settings (.cometcli/settings.json, settings.local.json, .mcp.json)
can define hooks and MCP servers — commands that run on this machine. They
are ignored until you trust the folder. Trust covers subdirectories.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if list {
				for _, d := range settings.Trusted() {
					fmt.Fprintln(cmd.OutOrStdout(), d)
				}
				return nil
			}
			dir, _ := os.Getwd()
			if len(args) == 1 {
				dir = args[0]
			} else if root := settings.FindRoot(dir); root != "" {
				dir = root
			}
			if err := settings.Trust(dir, !off); err != nil {
				return err
			}
			abs, _ := filepath.Abs(dir)
			verb := "trusted"
			if off {
				verb = "no longer trusted"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", verb, abs)
			return nil
		},
	}
	cmd.Flags().BoolVar(&off, "off", false, "revoke trust")
	cmd.Flags().BoolVar(&list, "list", false, "list trusted folders")
	return cmd
}
