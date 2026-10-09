package drill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/triage"
)

// Result is one scored drill.
type Result struct {
	Scenario   string        `json:"scenario"`
	Model      string        `json:"model"`
	TriageCase string        `json:"triage_case"`
	TriageOK   bool          `json:"triage_ok"` // triage named an expected root case
	Fixed      bool          `json:"fixed"`     // verified on the node after the agent finished
	FixDetail  string        `json:"fix_detail"`
	Steps      int           `json:"steps"`
	Tokens     int           `json:"tokens"`
	Duration   time.Duration `json:"duration_ns"`
	Report     string        `json:"report"`
	Err        string        `json:"error,omitempty"`
}

// AgentFactory builds an agent in general mode with the given approver.
type AgentFactory func(ctx context.Context, approve toolkit.Approver) (*agent.Agent, error)

// drillApprover approves everything — but only on drill profiles.
func drillApprover(c *toolkit.Context, prompt string, tier toolkit.Tier, _ map[string]any) (bool, error) {
	if c == nil || !IsDrill(c.Profile) {
		return false, fmt.Errorf("drill approvals only apply to drill profiles")
	}
	return true, nil
}

// topCase is triage's leading root case for the node.
func topCase(ctx context.Context, e *Env) string {
	c := &toolkit.Context{Context: ctx, Profile: e.Profile, Cfg: e.Cfg}
	defer c.Close()
	r := triage.Collect(c, 10*time.Minute)
	for _, h := range triage.LoadKB(c).Match(r.Signals, r.Chain) {
		if h.Case.Kind != "advisory" && h.SymptomOf == "" {
			return h.Case.ID
		}
	}
	return ""
}

// RunScenario injects the fault, has the agent work it, scores the
// outcome and recovers the node. log reports progress.
func RunScenario(ctx context.Context, e *Env, s Scenario, newAgent AgentFactory, log func(string)) (res Result) {
	res.Scenario = s.Name // named result: the deferred timing and recovery error must reach the caller
	if !IsDrill(e.Profile) {
		res.Err = "refusing: " + e.Profile.Name + " is not a drill profile"
		return res
	}
	start := time.Now()
	defer func() {
		res.Duration = time.Since(start)
		log("recovering " + e.Profile.Name)
		if err := e.Recover(ctx, func(ctx context.Context) error { return e.unjail(ctx) }); err != nil {
			res.Err = strings.TrimSpace(res.Err + "; recovery: " + err.Error())
		}
	}()
	log(fmt.Sprintf("injecting: %s", s.Desc))
	if err := s.Inject(ctx, e); err != nil {
		res.Err = "inject: " + err.Error()
		return res
	}
	if s.Ready != nil && !s.Ready(ctx, e) {
		res.Err = "the fault never became visible"
		return res
	}
	res.TriageCase = topCase(ctx, e)
	for _, want := range s.Expect {
		res.TriageOK = res.TriageOK || res.TriageCase == want
	}
	log(fmt.Sprintf("triage says %s (%v); the agent takes over", orDash(res.TriageCase), res.TriageOK))

	a, err := newAgent(ctx, drillApprover)
	if err != nil {
		res.Err = "agent: " + err.Error()
		return res
	}
	defer a.Close()
	res.Model = a.Provider.Name() + "/" + a.Model
	cmd, err := agent.RunCommand(a, a.Ctx, a.Reg, "/incident "+fmt.Sprintf(s.Prompt, e.Profile.Name))
	if err != nil {
		res.Err = "incident: " + err.Error()
		return res
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	report, err := a.Run(runCtx, cmd.Prompt)
	cancel()
	res.Report = report
	res.Steps = a.Rounds()
	u, _ := a.Usage()
	res.Tokens = u.Input + u.Output
	if err != nil {
		res.Err = "agent run: " + err.Error()
	}
	log("verifying on the node")
	ok := waitFor(ctx, 3*time.Minute, func() bool { v, _ := s.Verify(ctx, e); return v })
	res.Fixed = ok
	_, res.FixDetail = s.Verify(ctx, e)
	return res
}

// unjail sends MsgUnjail for the drill node through the normal tool.
func (e *Env) unjail(ctx context.Context) error {
	t, ok := e.Reg.Get("val.unjail")
	if !ok {
		return fmt.Errorf("val.unjail not registered")
	}
	c := &toolkit.Context{Context: ctx, Profile: e.Profile, Cfg: e.Cfg, Approver: drillApprover}
	defer c.Close()
	_, err := t.Run(c, toolkit.Args{})
	return err
}

// Save writes results to ~/.cometcli/drills/<time>.json.
func Save(results []Result) (string, error) {
	d, err := config.Path("drills")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(d, time.Now().Format("20060102-150405")+".json")
	raw, _ := json.MarshalIndent(results, "", "  ")
	return p, os.WriteFile(p, raw, 0o600)
}

// Scoreboard renders results as a table with totals.
func Scoreboard(results []Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-14s %-26s %-7s %-6s %6s %8s %7s\n", "scenario", "triage case", "triage", "fixed", "steps", "tokens", "time")
	triageOK, fixed := 0, 0
	for _, r := range results {
		mark := func(v bool) string {
			if v {
				return "✓"
			}
			return "✗"
		}
		fmt.Fprintf(&b, "%-14s %-26s %-7s %-6s %6d %8d %7s\n", r.Scenario, orDash(r.TriageCase), mark(r.TriageOK), mark(r.Fixed),
			r.Steps, r.Tokens, r.Duration.Round(time.Second))
		if r.TriageOK {
			triageOK++
		}
		if r.Fixed {
			fixed++
		}
		if r.Err != "" {
			fmt.Fprintf(&b, "  ! %s\n", r.Err)
		}
		if !r.Fixed && r.FixDetail != "" {
			fmt.Fprintf(&b, "  ↳ %s\n", r.FixDetail)
		}
	}
	fmt.Fprintf(&b, "score: triage %d/%d · fixed %d/%d", triageOK, len(results), fixed, len(results))
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
