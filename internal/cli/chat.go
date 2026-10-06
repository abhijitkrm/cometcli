package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tui/chatui"
)

// maxStdin bounds piped input; larger input keeps its head and tail.
const maxStdin = 200_000

// printFlags wires headless-mode flags (bare cometcli and `one`).
func printFlags(cmd *cobra.Command) {
	cmd.Flags().BoolP("print", "p", false, "answer once and exit — no TUI; for scripts, pipes and CI")
	cmd.Flags().String("output-format", "text", "with -p: text | json | stream-json")
	cmd.Flags().Bool("include-partial-messages", false, "with stream-json: also emit token deltas")
	cmd.Flags().Bool("verbose", false, "with -p text: show tool calls and notices on stderr")
	cmd.Flags().String("append-system-prompt", "", "extra instructions appended to the system prompt")
	cmd.Flags().Int("max-turns", 0, "max model rounds per prompt (same as --max-iter)")
}

// oneCmd is node mode: `cometcli one [profile] [prompt]`.
func oneCmd(reg *toolkit.Registry) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "one [profile] [prompt]",
		Short: "Node mode — chat scoped to one node profile (validator tools, safety rules, live snapshot)",
		Long: `Node mode scopes the session to one profile: the node tools (val, chain,
tx, upgrade, …) join the general ones, the shell runs on the node's host
(local or SSH), the validator safety rules apply, and a live snapshot
of the node rides along. Without a profile name the active profile is
used. Inside any session, /one <profile> and /one off switch modes.`,
		Args: cobra.ArbitraryArgs,
		ValidArgsFunction: func(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			cfg, err := config.Load()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var names []string
			for n := range cfg.Profiles {
				names = append(names, n)
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			name := flagProfile
			if len(args) > 0 {
				if _, ok := cfg.Profiles[args[0]]; ok {
					name, args = args[0], args[1:]
				}
			}
			p, err := cfg.ActiveProfile(name)
			if err != nil {
				return fmt.Errorf("%w — `cometcli one <profile>`, or `cometcli init` to add one", err)
			}
			return runChat(cmd, reg, p, args)
		},
	}
	agentFlags(cmd)
	printFlags(cmd)
	return cmd
}

// runChat is the shared entry for bare `cometcli` and `cometcli one`:
// interactive TUI on a terminal, headless (-p) otherwise. p == nil is
// general mode.
func runChat(cmd *cobra.Command, reg *toolkit.Registry, p *config.Profile, args []string) error {
	// `cometcli doctr` is a typo, not a prompt — checked only for a single
	// word typed interactively, long enough that a near miss is meaningful
	if p, _ := cmd.Flags().GetBool("print"); !p && len(args) == 1 && len(args[0]) >= 4 && !strings.ContainsAny(args[0], " \t?") {
		cmd.Root().SuggestionsMinimumDistance = 1
		if len(args[0]) >= 6 {
			cmd.Root().SuggestionsMinimumDistance = 2
		}
		if sug := cmd.Root().SuggestionsFor(args[0]); len(sug) > 0 {
			return fmt.Errorf("unknown command %q — did you mean %s? (quote a one-word prompt with a ?)", args[0], strings.Join(sug, ", "))
		}
	}
	prompt := strings.TrimSpace(strings.Join(args, " "))
	piped, err := readPipedStdin(cmd.InOrStdin())
	if err != nil {
		return err
	}
	if piped != "" {
		if prompt == "" {
			prompt = piped
		} else {
			prompt += "\n\n<stdin>\n" + piped + "\n</stdin>"
		}
	}
	printMode, _ := cmd.Flags().GetBool("print")
	headless := printMode || piped != "" || (!isTerminal(cmd.OutOrStdout()) && prompt != "")
	if !headless && !isTerminal(cmd.InOrStdin()) {
		if prompt == "" {
			return cmd.Help()
		}
		headless = true
	}

	c, err := chatCtx(cmd, p, headless)
	if err != nil {
		return err
	}
	defer c.Close()
	if c.Audit != nil {
		defer c.Audit.Close()
	}
	if headless {
		if prompt == "" {
			return fmt.Errorf("no prompt — pass one as an argument or pipe input in")
		}
		return runPrint(cmd, c, reg, prompt)
	}
	a, err := agent.New(c, reg)
	if err != nil {
		return err
	}
	defer a.Close()
	note, err := configureChatAgent(cmd, a)
	if err != nil {
		return err
	}
	var notes []string
	if note != "" {
		notes = append(notes, note)
	}
	return chatui.Run(chatui.Options{Agent: a, Prompt: prompt, Notes: notes})
}

// chatCtx builds the tool context for a chat session. Headless sessions
// never read approvals from stdin (it may be the piped input): anything
// needing approval is denied unless rules or the mode allow it.
func chatCtx(cmd *cobra.Command, p *config.Profile, headless bool) (*toolkit.Context, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	name := "general"
	if p != nil {
		name = p.Name
	}
	lg, _ := audit.Open(name)
	c := &toolkit.Context{
		Context: cmd.Context(), Profile: p, Cfg: cfg, Out: cmd.OutOrStdout(), Audit: lg,
		Approver: StdinApprover(cmd.InOrStdin()),
	}
	if headless {
		c.Out = cmd.ErrOrStderr()
		c.Approver = headlessApprover
	}
	if flagYes {
		c.AutoApproveBelow = toolkit.TierLocalChange
	}
	return c, nil
}

func headlessApprover(_ *toolkit.Context, prompt string, tier toolkit.Tier, _ map[string]any) (bool, error) {
	return false, fmt.Errorf("not approved: %s needs approval and -p has no one to ask — allow it with --allowedTools or --permission-mode, or run interactively", tier)
}

// configureChatAgent applies the agent flags plus the chat-only ones.
func configureChatAgent(cmd *cobra.Command, a *agent.Agent) (string, error) {
	if s, _ := cmd.Flags().GetString("append-system-prompt"); s != "" {
		a.AppendSystem = s
	}
	if n, _ := cmd.Flags().GetInt("max-turns"); n > 0 {
		a.MaxIter = n
	}
	return applyAgentFlags(cmd, a)
}

// readPipedStdin returns piped or redirected input. Terminals, /dev/null
// and other devices are skipped, so an idle stdin never blocks.
func readPipedStdin(r io.Reader) (string, error) {
	f, ok := r.(*os.File)
	if !ok {
		return "", nil
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&(os.ModeNamedPipe) == 0 && !fi.Mode().IsRegular() {
		return "", nil
	}
	b, err := io.ReadAll(io.LimitReader(f, 8*maxStdin))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if len(s) > maxStdin {
		head, tail := s[:maxStdin*2/3], s[len(s)-maxStdin/3:]
		s = head + fmt.Sprintf("\n…[%d bytes of input omitted]…\n", len(s)-len(head)-len(tail)) + tail
	}
	return s, nil
}

// runPrint answers one prompt headlessly in the requested format.
func runPrint(cmd *cobra.Command, c *toolkit.Context, reg *toolkit.Registry, prompt string) error {
	format, _ := cmd.Flags().GetString("output-format")
	switch format {
	case "text", "json", "stream-json":
	default:
		return fmt.Errorf("--output-format %q: want text, json or stream-json", format)
	}
	a, err := agent.New(c, reg)
	if err != nil {
		return err
	}
	defer a.Close()
	note, err := configureChatAgent(cmd, a)
	if err != nil {
		return err
	}
	if note != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), note)
	}
	out := cmd.OutOrStdout()
	enc := json.NewEncoder(out)
	partial, _ := cmd.Flags().GetBool("include-partial-messages")
	verbose, _ := cmd.Flags().GetBool("verbose")

	switch format {
	case "stream-json":
		_ = enc.Encode(map[string]any{
			"type": "system", "subtype": "init", "session_id": a.ID(), "cwd": a.WorkRoot,
			"provider": a.Provider.Name(), "model": a.Model, "mode": scopeName(a),
			"permission_mode": a.Policy.Mode, "tools": a.ToolNames(),
		})
		a.OnEvent = func(e agent.Event) {
			if line := streamEvent(e, partial); line != nil {
				_ = enc.Encode(line)
			}
		}
	case "text":
		if verbose {
			pr := &agent.Printer{Out: cmd.ErrOrStderr()}
			a.OnEvent = func(e agent.Event) {
				if e.Kind != agent.EvDelta && e.Kind != agent.EvText && e.Kind != agent.EvThinking {
					pr.Handle(e)
				}
			}
		}
	}
	// slash commands work headlessly too: `cometcli -p "/vote yes 7"`
	if strings.HasPrefix(prompt, "/") {
		cr, err := agent.RunCommand(a, c, reg, prompt)
		if err != nil {
			if errors.Is(err, agent.ErrUnknownCommand) {
				return fmt.Errorf("unknown command %s", strings.Fields(prompt)[0])
			}
			return err
		}
		if cr.Job != nil {
			txt, err := cr.Job(cmd.Context())
			if err != nil {
				return err
			}
			cr.Text = txt
		}
		if cr.Prompt == "" {
			if cr.Text != "" {
				fmt.Fprintln(out, cr.Text)
			}
			return nil
		}
		if verbose && cr.Text != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), cr.Text)
		}
		prompt = cr.Prompt
	}

	start := time.Now()
	res, runErr := a.Run(cmd.Context(), prompt)
	if format == "text" {
		if res != "" {
			fmt.Fprintln(out, res)
		}
		return runErr
	}
	usage, _ := a.Usage()
	result := map[string]any{
		"type": "result", "subtype": "success", "is_error": runErr != nil, "result": res,
		"session_id": a.ID(), "num_turns": a.Rounds(), "duration_ms": time.Since(start).Milliseconds(),
		"provider": a.Provider.Name(), "model": a.Model, "mode": scopeName(a),
		"usage": map[string]int{"input_tokens": usage.Input, "output_tokens": usage.Output, "cache_read_input_tokens": usage.CacheRead},
	}
	if runErr != nil {
		result["subtype"] = "error"
		result["error"] = runErr.Error()
		if errors.Is(runErr, cmd.Context().Err()) && cmd.Context().Err() != nil {
			result["subtype"] = "cancelled"
		}
	}
	_ = enc.Encode(result)
	return runErr
}

func scopeName(a *agent.Agent) string {
	if a.Node() {
		return "node:" + a.Ctx.Profile.Name
	}
	return "general"
}

// streamEvent maps an agent event to a stream-json line (nil = skip).
func streamEvent(e agent.Event, partial bool) map[string]any {
	switch e.Kind {
	case agent.EvDelta:
		if partial {
			return map[string]any{"type": "stream_event", "delta": e.Text}
		}
	case agent.EvText:
		return map[string]any{"type": "assistant", "text": e.Text}
	case agent.EvThinking:
		if partial {
			return map[string]any{"type": "thinking", "delta": e.Text}
		}
	case agent.EvToolStart:
		return map[string]any{"type": "tool_use", "tool": e.Tool, "tier": e.Tier, "input": e.Args}
	case agent.EvToolResult:
		m := map[string]any{"type": "tool_result", "tool": e.Tool, "is_error": e.Err != ""}
		if e.Err != "" {
			m["output"] = e.Err
		} else {
			m["output"] = e.Text
		}
		return m
	case agent.EvNotice:
		return map[string]any{"type": "notice", "text": e.Text}
	case agent.EvTodos:
		return map[string]any{"type": "todos", "todos": e.Todos}
	}
	return nil
}
