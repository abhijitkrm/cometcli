package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Check is one doctor line.
type Check struct {
	Name   string
	OK     bool
	Warn   bool
	Detail string
}

// DoctorCmd runs the full node health checklist.
func DoctorCmd(reg *toolkit.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Full validator health checklist",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := NewCtx(cmd, true)
			if err != nil {
				return err
			}
			defer c.Close()

			var checks []Check
			run := func(name string, tool string, args toolkit.Args, eval func(*toolkit.Result, error) Check) {
				t, ok := reg.Get(tool)
				if !ok {
					checks = append(checks, Check{name, false, false, "tool not registered"})
					return
				}
				res, err := t.Run(c, args)
				chk := eval(res, err)
				chk.Name = name
				checks = append(checks, chk)
			}

			run("rpc reachable", "node.status", nil, func(r *toolkit.Result, err error) Check {
				if err != nil {
					return Check{OK: false, Detail: err.Error()}
				}
				h, _ := r.Data["height"].(int64)
				return Check{OK: true, Detail: fmt.Sprintf("height %d", h)}
			})
			run("not catching up", "node.status", nil, func(r *toolkit.Result, err error) Check {
				if err != nil {
					return Check{OK: false, Warn: true, Detail: "unknown: " + err.Error()}
				}
				cu, _ := r.Data["catching_up"].(bool)
				return Check{OK: !cu, Warn: cu, Detail: fmt.Sprintf("catching_up=%v", cu)}
			})
			run("has peers", "node.peers", nil, func(r *toolkit.Result, err error) Check {
				if err != nil {
					return Check{OK: false, Detail: err.Error()}
				}
				n, _ := r.Data["count"].(int32)
				if n == 0 {
					return Check{OK: false, Detail: "0 peers"}
				}
				return Check{OK: true, Detail: fmt.Sprintf("%d peers", n)}
			})
			run("port exposure", "sec.exposure", nil, func(r *toolkit.Result, err error) Check {
				if err != nil {
					return Check{Warn: true, Detail: err.Error()}
				}
				crits, _ := r.Data["critical"].(int)
				if crits > 0 {
					return Check{OK: false, Detail: fmt.Sprintf("%d critical exposures", crits)}
				}
				return Check{OK: true, Detail: "clean"}
			})
			run("key file perms", "sec.perms", nil, func(r *toolkit.Result, err error) Check {
				if err != nil {
					return Check{Warn: true, Detail: err.Error()}
				}
				bad, _ := r.Data["bad"].(int)
				return Check{OK: bad == 0, Detail: fmt.Sprintf("%d problems", bad)}
			})
			run("signing health", "val.signing", nil, func(r *toolkit.Result, err error) Check {
				if err != nil {
					return Check{Warn: true, Detail: "skipped: " + err.Error()}
				}
				missed, _ := r.Data["missed"].(int64)
				window, _ := r.Data["window"].(int64)
				tomb, _ := r.Data["tombstoned"].(bool)
				if tomb {
					return Check{OK: false, Detail: "TOMBSTONED — double-sign slashed"}
				}
				if window > 0 && missed > window/10 {
					return Check{OK: false, Detail: fmt.Sprintf("missing %d/%d blocks", missed, window)}
				}
				return Check{OK: true, Detail: fmt.Sprintf("missed %d/%d", missed, window)}
			})
			run("jail status", "val.status", nil, func(r *toolkit.Result, err error) Check {
				if err != nil {
					return Check{Warn: true, Detail: "skipped: " + err.Error()}
				}
				j, _ := r.Data["jailed"].(bool)
				if j {
					return Check{OK: false, Detail: "JAILED — investigate + `cometcli val unjail`"}
				}
				return Check{OK: true, Detail: "not jailed"}
			})
			run("upgrade pending", "chain.upgrade-plan", nil, func(r *toolkit.Result, err error) Check {
				if err != nil {
					return Check{Warn: true, Detail: "skipped: " + err.Error()}
				}
				if r.Data["plan"] == nil {
					return Check{OK: true, Detail: "none"}
				}
				p := r.Data["plan"].(map[string]any)
				return Check{Warn: true, Detail: fmt.Sprintf("UPGRADE %q at height %v", p["name"], p["height"])}
			})
			run("host disk", "node.health", nil, func(r *toolkit.Result, err error) Check {
				if err != nil {
					return Check{Warn: true, Detail: err.Error()}
				}
				return Check{OK: true}
			})
			run("ntp/clock", "node.health", nil, func(r *toolkit.Result, err error) Check {
				return Check{OK: true, Detail: "see service output"}
			})
			if c.Profile.Endpoints.EVM != "" {
				run("evm parity", "evm.parity", nil, func(r *toolkit.Result, err error) Check {
					if err != nil {
						return Check{Warn: true, Detail: err.Error()}
					}
					d, _ := r.Data["drift"].(int64)
					if d > 10 {
						return Check{OK: false, Detail: fmt.Sprintf("JSON-RPC lagging %d blocks", d)}
					}
					return Check{OK: true, Detail: fmt.Sprintf("drift %d", d)}
				})
			}

			// render
			var b strings.Builder
			fails, warns := 0, 0
			for _, ch := range checks {
				icon := "✓"
				switch {
				case !ch.OK && ch.Detail != "":
					icon = "✗"
					fails++
				case ch.Warn:
					icon = "!"
					warns++
				case !ch.OK:
					icon = "✗"
					fails++
				}
				fmt.Fprintf(&b, "%s  %-18s %s\n", icon, ch.Name, ch.Detail)
			}
			fmt.Fprintf(&b, "\n%d failures, %d warnings\n", fails, warns)
			fmt.Fprint(c.Out, b.String())
			return nil
		},
	}
}
