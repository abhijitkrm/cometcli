package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/silence"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/triage"
	"github.com/abhijitkrm/cometcli/internal/watch"
)

func watchCmd(reg *toolkit.Registry) *cobra.Command {
	var interval, mode, cooldown, listen string
	var profiles []string
	var once bool
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Watch the fleet on your behalf: triage on an interval, work new incidents, report and ask for approvals remotely",
		Long: `Sweeps every node (fleet triage) on an interval. A newly appearing critical or
high problem becomes an incident:

  notify    report the finding
  diagnose  the agent investigates read-only and reports (default)
  fix       the agent may act; every approval is sent to Telegram with
            Approve / Deny buttons (unanswered = denied)

Findings, reports and recoveries go to the watch.alerts channels (Slack,
Discord, Telegram). Transactions are refused unless watch.allow_tx is set.
Configure under 'watch:' in ~/.cometcli/config.yaml; flags override it.

Optionally, --listen receives a monitor's alerts (cometduty's webhook or
Alertmanager's) and works each at once: the node it names is triaged right
away instead of at the next sweep. Requests must be signed with the
WATCH_WEBHOOK_SECRET credential (cometduty's webhook.secret) or carry the
WATCH_WEBHOOK_TOKEN bearer token (Alertmanager's http_config). Sweeps keep
running either way.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			wc := cfg.Watch
			pick := func(flag, conf, def string) string {
				if cmd.Flags().Changed(flag) {
					return map[string]string{"interval": interval, "mode": mode, "cooldown": cooldown}[flag]
				}
				if conf != "" {
					return conf
				}
				return def
			}
			iv, err := time.ParseDuration(pick("interval", wc.Interval, "5m"))
			if err != nil || iv < 30*time.Second {
				return fmt.Errorf("interval: want a duration ≥ 30s")
			}
			cd, err := time.ParseDuration(pick("cooldown", wc.Cooldown, "1h"))
			if err != nil {
				return fmt.Errorf("cooldown: %w", err)
			}
			m, err := watch.ParseMode(pick("mode", wc.Mode, ""))
			if err != nil {
				return err
			}
			names := wc.Profiles
			if cmd.Flags().Changed("profiles") {
				names = profiles
			}
			selected := map[string]*config.Profile{}
			var list []*config.Profile
			var all []string
			for n := range cfg.Profiles {
				all = append(all, n)
			}
			sort.Strings(all)
			for _, n := range all {
				if len(names) > 0 && !contains(names, n) {
					continue
				}
				p := cfg.Profiles[n]
				p.Name = n
				selected[n] = p
				list = append(list, p)
			}
			if len(list) == 0 {
				return fmt.Errorf("no profiles to watch — cometcli init")
			}

			alerts := wc.Alerts
			if alerts.TelegramToken == "" {
				alerts.TelegramToken = config.Credential("TELEGRAM_BOT_TOKEN")
			}
			var notify []watch.Notifier
			var channels []string
			for _, s := range monitor.Sinks(alerts) {
				notify = append(notify, s)
				channels = append(channels, s.Name())
			}
			var approver toolkit.Approver
			if m == watch.ModeFix {
				if alerts.TelegramToken == "" || alerts.TelegramChatID == "" {
					fmt.Fprintln(cmd.ErrOrStderr(), "fix mode needs Telegram for approvals (watch.alerts.telegram_token or the TELEGRAM_BOT_TOKEN credential, and telegram_chat_id) — running read-only")
				} else {
					at, _ := time.ParseDuration(wc.ApprovalTimeout)
					users := map[int64]bool{}
					for _, u := range wc.TelegramUsers {
						users[u] = true
					}
					tg := &watch.Telegram{Token: alerts.TelegramToken, ChatID: alerts.TelegramChatID, Users: users, Timeout: at}
					approver = watch.RemoteApprover(tg.Ask, wc.AllowTx)
				}
			}
			var alertsCh chan watch.Alert
			addr := wc.Listen
			if cmd.Flags().Changed("listen") {
				addr = listen
			}
			if addr != "" {
				rcv := &watch.Receiver{Secret: credential("WATCH_WEBHOOK_SECRET"), Token: credential("WATCH_WEBHOOK_TOKEN")}
				if rcv.Secret == "" && rcv.Token == "" {
					return fmt.Errorf("--listen needs the WATCH_WEBHOOK_SECRET or WATCH_WEBHOOK_TOKEN credential (cometcli config set-key …) so only your monitor can send alerts")
				}
				alertsCh = make(chan watch.Alert, 64)
				rcv.Out = alertsCh
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					return fmt.Errorf("listen %s: %w", addr, err)
				}
				mux := http.NewServeMux()
				mux.Handle("/alerts", rcv)
				srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second}
				go func() { _ = srv.Serve(ln) }()
				defer srv.Close()
				fmt.Fprintf(cmd.OutOrStdout(), "receiving monitor alerts on http://%s/alerts\n", ln.Addr())
			}
			statePath, _ := config.Path("watch-state.json")
			glog, _ := audit.Open("watch")
			w := &watch.Watcher{
				Mode: m, Cooldown: cd, Profiles: selected, Notify: notify, Approver: approver,
				Planned: func(ctx context.Context, p *config.Profile) string {
					c := &toolkit.Context{Context: ctx, Profile: p, Cfg: cfg, Audit: glog}
					defer c.Close()
					return silence.Planned(c, cfg, p)
				},
				StatePath: statePath, Out: cmd.OutOrStdout(),
				Collect: func(ctx context.Context) []triage.NodeReport {
					return triage.CollectFleet(&toolkit.Context{Context: ctx, Cfg: cfg, Audit: glog}, list, iv)
				},
				Alerts: alertsCh,
				CollectOne: func(ctx context.Context, p *config.Profile) []triage.NodeReport {
					return triage.CollectFleet(&toolkit.Context{Context: ctx, Cfg: cfg, Audit: glog}, []*config.Profile{p}, iv)
				},
				Run: func(ctx context.Context, p *config.Profile, desc string, readOnly bool, approve toolkit.Approver) (string, error) {
					lg, _ := audit.Open(p.Name)
					if lg != nil {
						defer lg.Close()
					}
					c := &toolkit.Context{Context: ctx, Profile: p, Cfg: cfg, Audit: lg, Approver: approve}
					a, err := agent.New(c, reg)
					if err != nil {
						return "", err
					}
					defer a.Close()
					if readOnly {
						a.Policy.Mode = agent.ModeReadOnly
					}
					res, err := agent.RunCommand(a, a.Ctx, reg, "/incident "+desc)
					if err != nil {
						return "", err
					}
					return a.Run(ctx, res.Prompt)
				},
			}
			if len(channels) == 0 {
				channels = []string{"this terminal only"}
			}
			var watched []string
			for _, p := range list {
				watched = append(watched, p.Name)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "watching %d nodes (%s) every %s · mode %s · cooldown %s · reports to %s\n",
				len(list), strings.Join(watched, ", "), iv, m, cd, strings.Join(channels, ", "))
			w.Loop(cmd.Context(), iv, once)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&interval, "interval", "", "time between sweeps (default 5m, or watch.interval)")
	f.StringVar(&mode, "mode", "", "notify | diagnose | fix (default diagnose, or watch.mode)")
	f.StringVar(&cooldown, "cooldown", "", "before the same issue is worked again (default 1h)")
	f.StringSliceVar(&profiles, "profiles", nil, "profiles to watch (default all, or watch.profiles)")
	f.BoolVar(&once, "once", false, "run one sweep and exit")
	f.StringVar(&listen, "listen", "", "also receive monitor alerts on this address, e.g. 127.0.0.1:9180 (or watch.listen)")
	return cmd
}

// credential reads a secret from the environment, else ~/.cometcli/credentials.
func credential(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return config.Credential(name)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
