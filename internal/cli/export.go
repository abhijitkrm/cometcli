package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/silence"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/networktool"
)

func networkExportCmd() *cobra.Command {
	var format, layout, rpcs, profiles, out, webhookURL, chainOn, image, bind, tgChat string
	var silenceAPI, deploy, link, telegram bool
	cmd := &cobra.Command{
		Use:   "export <chain-id> --format cometduty",
		Short: "Write monitoring config for a network's nodes (cometduty), optionally deploy it",
		Long: `Writes cometduty monitoring for the nodes of <chain-id>, from what the
profiles already know (RPC, EVM and metrics ports, node home, container,
operator address). Every alert carries the label profile=<name>.

  --layout sidecar     (default) a cometduty beside each node — everything,
                       including disk, signer-state and log alerts that need
                       the node's machine — plus a chain-role instance
  --layout standalone  one cometduty for the whole network on the --chain-on
                       host: every validator's signing, the active set, chain
                       health, and each node whose RPC it can reach; no disk,
                       signer-state or log alerts. --rpc adds endpoints it
                       can reach (a public RPC)

  --webhook-url   send alerts to a receiver, e.g. cometcli watch --listen;
                  signed with a secret saved as WATCH_WEBHOOK_SECRET
  --telegram      send every alert to Telegram too, with the bot and chat
                  watch already uses (watch.alerts, TELEGRAM_BOT_TOKEN);
                  --telegram-chat picks another chat
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
			if telegram || tgChat != "" {
				o.TelegramToken, o.TelegramChat = cfg.Watch.Alerts.TelegramToken, cfg.Watch.Alerts.TelegramChatID
				if o.TelegramToken == "" {
					o.TelegramToken = credential("TELEGRAM_BOT_TOKEN")
				}
				if tgChat != "" {
					o.TelegramChat = tgChat
				}
				if o.TelegramToken == "" || o.TelegramChat == "" {
					return fmt.Errorf("--telegram needs a bot token (watch.alerts.telegram_token or the TELEGRAM_BOT_TOKEN credential) and a chat (watch.alerts.telegram_chat_id or --telegram-chat)")
				}
			}
			var sets []networktool.MonitorFiles
			switch layout {
			case "sidecar":
				if rpcs != "" {
					return fmt.Errorf("--rpc is for --layout standalone")
				}
				if sets, err = networktool.CometdutyFiles(chain, list, o); err != nil {
					return err
				}
			case "standalone":
				f, skipped, err := networktool.StandaloneFiles(chain, list, o, splitCSV(rpcs))
				if err != nil {
					return err
				}
				sets = []networktool.MonitorFiles{f}
				if len(skipped) > 0 {
					fmt.Fprintf(w, "note: %s not watched as nodes — their RPC is loopback-only on another host (signing is still watched for validators)\n", strings.Join(skipped, ", "))
				}
			default:
				return fmt.Errorf("--layout: sidecar or standalone")
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
					on = chainHost.Name + " (" + map[string]string{"sidecar": "chain-role", "standalone": "standalone"}[layout] + " instance)"
				}
				fmt.Fprintf(w, "  %-28s → %s  (metrics :%d)\n", on, dir, f.Port)
			}
			if layout == "standalone" {
				fmt.Fprintf(w, "✓ cometduty standalone config for %s (%d profiles) in %s\n", chain, len(list), out)
			} else {
				fmt.Fprintf(w, "✓ cometduty files for %d nodes of %s in %s\n", len(list), chain, out)
			}

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
						who = layout + " instance on " + p.Name
						if layout == "sidecar" {
							who = "chain instance on " + p.Name
						}
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
				if layout == "standalone" {
					fmt.Fprintln(w, "✓ linked: the standalone instance under monitor.silence (planned work silences it)")
				} else {
					fmt.Fprintln(w, "✓ linked: each profile's endpoints.monitor and the chain instance under monitor.silence")
				}
			}
			var next []string
			if !deploy {
				if layout == "standalone" {
					next = append(next, "copy the folder to any machine that reaches those RPCs and run `docker compose up -d` there — or rerun with --deploy")
				} else {
					next = append(next, "copy each folder to its node's host and run `USER_ID=$(stat -c %u:%g <node home>) docker compose up -d` there — or rerun with --deploy")
				}
			}
			if webhookURL != "" {
				next = append(next, "receive the alerts: cometcli watch --listen <addr> (reachable at "+webhookURL+")")
			}
			if !link {
				if layout == "standalone" {
					next = append(next, "rerun with --link so cometcli silences it during planned work")
				} else {
					next = append(next, "rerun with --link so triage reads each node's monitor and cometcli silences it during planned work")
				}
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
	f.StringVar(&layout, "layout", "sidecar", "sidecar (one per node + chain instance) or standalone (one for the network)")
	f.StringVar(&rpcs, "rpc", "", "standalone: extra RPC endpoints it can reach, comma-separated (data sources, not paged)")
	f.StringVar(&profiles, "profiles", "", "nodes to include (default: every profile on <chain-id>)")
	f.StringVar(&out, "out", "", "where to write (default ~/.cometcli/monitor/<chain-id>)")
	f.StringVar(&webhookURL, "webhook-url", "", "send alerts here, signed (e.g. where cometcli watch --listen is reachable)")
	f.BoolVar(&telegram, "telegram", false, "send alerts to Telegram too (watch's bot and chat)")
	f.StringVar(&tgChat, "telegram-chat", "", "Telegram chat id for alerts (implies --telegram)")
	f.BoolVar(&silenceAPI, "silence-api", false, "enable cometduty's silence API (token in the MONITOR_API_TOKEN credential)")
	f.StringVar(&chainOn, "chain-on", "", "profile whose host runs the chain-role (or standalone) instance (default: the first)")
	f.StringVar(&image, "image", "", "cometduty image (default "+networktool.CometdutyImage+")")
	f.StringVar(&bind, "metrics-bind", "127.0.0.1", "where cometduty's /metrics and silence API listen (0.0.0.0 for a remote Prometheus)")
	f.BoolVar(&deploy, "deploy", false, "copy the files to each host and start cometduty there")
	f.BoolVar(&link, "link", false, "record the monitors in cometcli's config (endpoints.monitor, monitor.silence)")
	return cmd
}
