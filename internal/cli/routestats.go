package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/router"
)

func routeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "route", Short: "How prompts were answered: locally or by the model"}
	var since time.Duration
	var top int
	stats := &cobra.Command{
		Use:   "stats",
		Short: "Share of prompts answered without the model, and what the model keeps being asked",
		Long: `Reads the audit log. Local answers are known questions, direct commands
and known incidents with a playbook; the rest went to the model. The
prompts the model gets most often are what to teach cometcli next: an
intent in ~/.cometcli/intents, or a playbook (incidents offer to save one).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := RouteStats(time.Now().Add(-since))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			total := s.Local + s.Model
			if total == 0 {
				fmt.Fprintln(out, "no prompts in the audit log for that period")
				return nil
			}
			fmt.Fprintf(out, "last %s: %d prompts — %d answered locally (%.0f%%), %d by the model\n",
				since, total, s.Local, 100*float64(s.Local)/float64(total), s.Model)
			if len(s.ByIntent) > 0 {
				fmt.Fprintln(out, "\nlocal, by intent or case:")
				for _, kv := range sorted(s.ByIntent, top) {
					fmt.Fprintf(out, "  %4d  %s\n", kv.n, kv.k)
				}
			}
			if len(s.ModelPrompts) > 0 {
				fmt.Fprintln(out, "\nwhat the model was asked most (candidates to answer locally):")
				for _, kv := range sorted(s.ModelPrompts, top) {
					fmt.Fprintf(out, "  %4d  %s\n", kv.n, kv.k)
				}
			}
			return nil
		},
	}
	stats.Flags().DurationVar(&since, "since", 7*24*time.Hour, "how far back")
	stats.Flags().IntVar(&top, "top", 10, "rows per list")
	cmd.AddCommand(stats)
	return cmd
}

// Stats are routing counts from the audit log.
type Stats struct {
	Local, Model int
	ByIntent     map[string]int
	ModelPrompts map[string]int // normalized prompt → count
}

// RouteStats tallies prompt events since t.
func RouteStats(since time.Time) (*Stats, error) {
	dir, err := config.Path("audit")
	if err != nil {
		return nil, err
	}
	s := &Stats{ByIntent: map[string]int{}, ModelPrompts: map[string]int{}}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, f := range files {
		day := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if day < since.UTC().Format("2006-01-02") {
			continue
		}
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var e audit.Event
			if json.Unmarshal(sc.Bytes(), &e) != nil || e.Kind != audit.KindPrompt || e.TS.Before(since) {
				continue
			}
			text, _ := e.Detail["text"].(string)
			switch e.Detail["route"] {
			case "local":
				s.Local++
				key, _ := e.Detail["intent"].(string)
				if c, _ := e.Detail["case"].(string); c != "" {
					key = "incident " + c
				}
				s.ByIntent[key]++
			case "model":
				// procedures (/incident) are logged expanded: count their first line
				if strings.HasPrefix(text, "Work this incident") {
					text = "/incident (no playbook for it yet)"
				}
				s.Model++
				p := router.Normalize(text)
				if len(p) > 70 {
					p = p[:70] + "…"
				}
				s.ModelPrompts[p]++
			}
		}
		fh.Close()
	}
	return s, nil
}

type kv struct {
	k string
	n int
}

func sorted(m map[string]int, top int) []kv {
	var out []kv
	for k, n := range m {
		out = append(out, kv{k, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].k < out[j].k
	})
	if len(out) > top {
		out = out[:top]
	}
	return out
}
