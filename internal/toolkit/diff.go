package toolkit

import (
	"fmt"
	"strings"
)

// maxDiffLines bounds the LCS table (n*m) — config files are far smaller.
const maxDiffLines = 4000

// Diff renders a compact unified-style line diff of a file edit for
// approval prompts: changed lines with one line of context, hunks
// separated by "…". Returns "" when nothing changed.
func Diff(path, before, after string) string {
	if before == after {
		return ""
	}
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	if len(a) > maxDiffLines || len(b) > maxDiffLines {
		return fmt.Sprintf("--- %s\n(file too large to diff: %d → %d lines)", path, len(a), len(b))
	}
	// LCS lengths from the end
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		kind byte // ' ', '-', '+'
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, op{'+', b[j]})
	}
	// keep changes plus one line of context
	keep := make([]bool, len(ops))
	for k, o := range ops {
		if o.kind != ' ' {
			for d := -1; d <= 1; d++ {
				if k+d >= 0 && k+d < len(ops) {
					keep[k+d] = true
				}
			}
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n", path)
	gap := false
	for k, o := range ops {
		if !keep[k] {
			gap = true
			continue
		}
		if gap && sb.Len() > 0 {
			sb.WriteString("…\n")
		}
		gap = false
		fmt.Fprintf(&sb, "%c %s\n", o.kind, o.text)
	}
	return strings.TrimRight(sb.String(), "\n")
}
