package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/triage"
	"github.com/abhijitkrm/cometcli/internal/watch"
)

func watchCmd(reg *toolkit.Registry) *cobra.Command {
	var interval, mode, cooldown string
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
Configure under 'watch:' in ~/.cometcli/config.yaml; flags override it.`,
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
			statePath, _ := config.Path("watch-state.json")
			glog, _ := audit.Open("watch")
			w := &watch.Watcher{
				Mode: m, Cooldown: cd, Profiles: selected, Notify: notify, Approver: approver,
				StatePath: statePath, Out: cmd.OutOrStdout(),
				Collect: func(ctx context.Context) []triage.NodeReport {
					return triage.CollectFleet(&toolkit.Context{Context: ctx, Cfg: cfg, Audit: glog}, list, iv)
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
	return cmd
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
