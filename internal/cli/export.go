package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/silence"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/networktool"
)

func networkExportCmd() *cobra.Command {
	var format, profiles, out, webhookURL, chainOn, image, bind string
	var silenceAPI, deploy, link bool
	cmd := &cobra.Command{
		Use:   "export <chain-id> --format cometduty",
		Short: "Write monitoring config for a network's nodes (cometduty), optionally deploy it",
		Long: `Writes a cometduty sidecar for each node of <chain-id> — config and
compose file, from what the profiles already know (RPC, EVM and metrics
ports, node home, container, operator address) — plus the chain-role
instance, beside one node. Every alert carries the label profile=<name>.

  --webhook-url   send alerts to a receiver, e.g. cometcli watch --listen;
                  signed with a secret saved as WATCH_WEBHOOK_SECRET
  --silence-api   turn on cometduty's silence API (token saved as
                  MONITOR_API_TOKEN) so cometcli silences a node's alerts
                  during planned work
  --deploy        copy each node's files to its host and start cometduty
                  there (docker compose), after you confirm
  --link          record each node's monitor (endpoints.monitor) and the
                  chain instance (monitor.silence) in cometcli's config

Without --deploy nothing leaves this machine: the files are in --out.
cometduty doesn't need cometcli, and cometcli doesn't need cometduty.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			chain := args[0]
			if format != "cometduty" {
				return fmt.Errorf("--format: only cometduty is supported")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			var list []*config.Profile
			want := splitCSV(profiles)
			var names []string
			for n := range cfg.Profiles {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				p := cfg.Profiles[n]
				if (len(want) > 0 && !contains(want, n)) || (len(want) == 0 && p.ChainID != chain) {
					continue
				}
				p.Name = n
				list = append(list, p)
			}
			for _, n := range want {
				if cfg.Profiles[n] == nil {
					return fmt.Errorf("no profile %s", n)
				}
			}
			if len(list) == 0 {
				return fmt.Errorf("no profiles on %s", chain)
			}
			o := networktool.MonitorOpts{Image: image, WebhookURL: webhookURL, ChainOn: chainOn, MetricsBind: bind}
			if webhookURL != "" {
				if o.Secret = credential("WATCH_WEBHOOK_SECRET"); o.Secret == "" {
					o.Secret = networktool.RandomSecret()
					if err := config.SetCredential("WATCH_WEBHOOK_SECRET", o.Secret); err != nil {
						return err
					}
					fmt.Fprintln(w, "✓ new webhook secret saved as the WATCH_WEBHOOK_SECRET credential")
				}
			}
			if silenceAPI {
				if o.APIToken = credential(silence.TokenCredential); o.APIToken == "" {
					o.APIToken = networktool.RandomSecret()
					if err := config.SetCredential(silence.TokenCredential, o.APIToken); err != nil {
						return err
					}
					fmt.Fprintf(w, "✓ new silence API token saved as the %s credential\n", silence.TokenCredential)
				}
			}
			sets, err := networktool.CometdutyFiles(chain, list, o)
			if err != nil {
				return err
			}
			if out == "" {
				if out, err = config.Path(filepath.Join("monitor", netspec.SafeName(chain))); err != nil {
					return err
				}
			}
			chainHost := networktool.ChainHost(list, chainOn)
			for _, f := range sets {
				dir := filepath.Join(out, f.Dir)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				for name, content := range f.Files {
					if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
						return err
					}
				}
				on := f.Profile
				if on == "" {
					on = chainHost.Name + " (chain-role instance)"
				}
				fmt.Fprintf(w, "  %-28s → %s  (metrics :%d)\n", on, dir, f.Port)
			}
			fmt.Fprintf(w, "✓ cometduty files for %d nodes of %s in %s\n", len(list), chain, out)

			if deploy {
				if !flagYes && !confirm(cmd, fmt.Sprintf("start cometduty on the hosts of %d nodes (docker compose, as each node home's owner)?", len(list))) {
					return fmt.Errorf("cancelled — the files are in %s", out)
				}
				for _, f := range sets {
					p := chainHost
					if f.Profile != "" {
						p = cfg.Profiles[f.Profile]
					}
					c := &toolkit.Context{Context: cmd.Context(), Profile: p, Cfg: cfg,
						Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { return true, nil }}
					msg, err := networktool.DeployMonitor(c, chain, f, p.Home)
					c.Close()
					if err != nil {
						return fmt.Errorf("%s: %w", p.Name, err)
					}
					who := f.Profile
					if who == "" {
						who = "chain instance on " + p.Name
					}
					fmt.Fprintf(w, "✓ %s: %s\n", who, msg)
				}
			}
			if link {
				for _, f := range sets {
					if f.Profile != "" {
						cfg.Profiles[f.Profile].Endpoints.Monitor = fmt.Sprintf("http://127.0.0.1:%d", f.Port)
					} else {
						u := fmt.Sprintf("http://127.0.0.1:%d", f.Port)
						var kept []config.SilenceAPI
						for _, s := range cfg.Monitor.Silence {
							if s.Via != chainHost.Name || s.URL != u {
								kept = append(kept, s)
							}
						}
						cfg.Monitor.Silence = append(kept, config.SilenceAPI{URL: u, Via: chainHost.Name})
					}
				}
				if err := cfg.Save(); err != nil {
					return err
				}
				fmt.Fprintln(w, "✓ linked: each profile's endpoints.monitor and the chain instance under monitor.silence")
			}
			var next []string
			if !deploy {
				next = append(next, "copy each folder to its node's host and run `USER_ID=$(stat -c %u:%g <node home>) docker compose up -d` there — or rerun with --deploy")
			}
			if webhookURL != "" {
				next = append(next, "receive the alerts: cometcli watch --listen <addr> (reachable at "+webhookURL+")")
			}
			if !link {
				next = append(next, "rerun with --link so triage reads each node's monitor and cometcli silences it during planned work")
			}
			for i, n := range next {
				fmt.Fprintf(w, "next %d: %s\n", i+1, n)
			}
			if !silenceAPI && link {
				fmt.Fprintln(w, "note: without --silence-api cometduty serves no silence API; restarts and upgrades will page as usual")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&format, "format", "cometduty", "what to write (cometduty)")
	f.StringVar(&profiles, "profiles", "", "nodes to include (default: every profile on <chain-id>)")
	f.StringVar(&out, "out", "", "where to write (default ~/.cometcli/monitor/<chain-id>)")
	f.StringVar(&webhookURL, "webhook-url", "", "send alerts here, signed (e.g. where cometcli watch --listen is reachable)")
	f.BoolVar(&silenceAPI, "silence-api", false, "enable cometduty's silence API (token in the MONITOR_API_TOKEN credential)")
	f.StringVar(&chainOn, "chain-on", "", "profile whose host runs the chain-role instance (default: the first)")
	f.StringVar(&image, "image", "", "cometduty image (default "+networktool.CometdutyImage+")")
	f.StringVar(&bind, "metrics-bind", "127.0.0.1", "where cometduty's /metrics and silence API listen (0.0.0.0 for a remote Prometheus)")
	f.BoolVar(&deploy, "deploy", false, "copy the files to each host and start cometduty there")
	f.BoolVar(&link, "link", false, "record the monitors in cometcli's config (endpoints.monitor, monitor.silence)")
	return cmd
}
