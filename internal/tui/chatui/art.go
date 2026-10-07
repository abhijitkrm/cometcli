package chatui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The startup art: a comet streaking over "COMETCLI" in block letters,
// the way Claude Code greets you with its name. Drawn only when the
// terminal is wide enough; colors degrade with the terminal's profile.

// glyphs is the ANSI Shadow figlet font, for the letters we need.
var glyphs = map[rune][6]string{
	'C': {" ██████╗", "██╔════╝", "██║     ", "██║     ", "╚██████╗", " ╚═════╝"},
	'O': {" ██████╗ ", "██╔═══██╗", "██║   ██║", "██║   ██║", "╚██████╔╝", " ╚═════╝ "},
	'M': {"███╗   ███╗", "████╗ ████║", "██╔████╔██║", "██║╚██╔╝██║", "██║ ╚═╝ ██║", "╚═╝     ╚═╝"},
	'E': {"███████╗", "██╔════╝", "█████╗  ", "██╔══╝  ", "███████╗", "╚══════╝"},
	'T': {"████████╗", "╚══██╔══╝", "   ██║   ", "   ██║   ", "   ██║   ", "   ╚═╝   "},
	'L': {"██╗     ", "██║     ", "██║     ", "██║     ", "███████╗", "╚══════╝"},
	'I': {"██╗", "██║", "██║", "██║", "██║", "╚═╝"},
}

// wordmark renders s in the block font (rows of equal width).
func wordmark(s string) []string {
	rows := make([]string, 6)
	for _, r := range s {
		g := glyphs[r]
		for i := range rows {
			rows[i] += g[i]
		}
	}
	return rows
}

// comet is the streak above the wordmark: a glowing nucleus and a tail
// of five strands that flares wider the further it trails, in a field
// of stars. Every rune is one column wide.
var comet = []string{
	`         ˚                    ·                    ⋆                ·`,
	`            ·  ·  ∙  ∙ ∙ ∙ ∙ ∙ ∙ ∙ ─────────━━━━                ✦`,
	`      ·  ·  ∙  ∙ ∙ ∙ ∙ ─────────━━━━━━━━━━━━━━━━━━━━━━━━━━━░▒▓▄`,
	`  ·  ·  ∙  ∙ ∙ ─────────━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━▒▓██●`,
	`      ·  ·  ∙  ∙ ∙ ∙ ∙ ─────────━━━━━━━━━━━━━━━━━━━━━━━━━━━░▒▓▀`,
	`            ·  ·  ∙  ∙ ∙ ∙ ∙ ∙ ∙ ∙ ─────────━━━━                 ˙`,
	`                      ⋆                     ˚                     ·`,
}

type rgb struct{ r, g, b float64 }

func hex(s string) rgb {
	var c rgb
	var r, g, b int
	fmt.Sscanf(strings.TrimPrefix(s, "#"), "%02x%02x%02x", &r, &g, &b)
	c.r, c.g, c.b = float64(r), float64(g), float64(b)
	return c
}

// along returns the color at t ∈ [0,1] on a multi-stop gradient.
func along(stops []rgb, t float64) lipgloss.Color {
	if t <= 0 {
		t = 0
	}
	if t >= 1 {
		t = 1
	}
	seg := t * float64(len(stops)-1)
	i := int(seg)
	if i >= len(stops)-1 {
		i = len(stops) - 2
	}
	f := seg - float64(i)
	a, b := stops[i], stops[i+1]
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x",
		int(a.r+(b.r-a.r)*f), int(a.g+(b.g-a.g)*f), int(a.b+(b.b-a.b)*f)))
}

var (
	// tail: deep violet far behind, through blue and cyan, to white-hot
	tailStops = []rgb{hex("#3B2A6B"), hex("#6A4FC9"), hex("#4F8EF7"), hex("#7FE3F5"), hex("#F2FBFF")}
	// wordmark: comet gold through Claude-ish orange to violet
	wordStops = []rgb{hex("#FFD27A"), hex("#F09A5B"), hex("#D97757"), hex("#C2609E"), hex("#8E6BE8")}
	headSt    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFF7E0")).Bold(true)
	sparkSt   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD27A"))
	starSt    = lipgloss.NewStyle().Foreground(lipgloss.Color("#6B6F8A"))
)

// paint colors each rune of rows by its column along stops.
func paint(rows []string, stops []rgb, style func(r rune, c lipgloss.Color) string) []string {
	width := 0
	for _, row := range rows {
		width = max(width, len([]rune(row)))
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		var b strings.Builder
		for x, r := range []rune(row) {
			if r == ' ' {
				b.WriteRune(' ')
				continue
			}
			b.WriteString(style(r, along(stops, float64(x)/float64(max(1, width-1)))))
		}
		out[i] = b.String()
	}
	return out
}

// artWidth is the widest line of the art.
func artWidth() int {
	w := 0
	for _, l := range append(append([]string{}, comet...), wordmark("COMETCLI")...) {
		w = max(w, len([]rune(l)))
	}
	return w
}

// cometArt returns the startup art, or "" when the terminal is narrower
// than the art (it would wrap into noise).
func cometArt(termWidth int) string {
	if termWidth < artWidth()+2 {
		return ""
	}
	tail := paint(comet[1:6], tailStops, func(r rune, c lipgloss.Color) string {
		switch r {
		case '●':
			return headSt.Render("●")
		case '✦':
			return sparkSt.Render("✦")
		}
		return lipgloss.NewStyle().Foreground(c).Render(string(r))
	})
	stars := func(row string) string {
		var b strings.Builder
		for _, r := range row {
			switch r {
			case ' ':
				b.WriteRune(' ')
			case '✦':
				b.WriteString(sparkSt.Render("✦"))
			default:
				b.WriteString(starSt.Render(string(r)))
			}
		}
		return b.String()
	}
	sky := append(append([]string{stars(comet[0])}, tail...), stars(comet[6]))
	word := paint(wordmark("COMETCLI"), wordStops, func(r rune, c lipgloss.Color) string {
		st := lipgloss.NewStyle().Foreground(c)
		if r == '█' {
			st = st.Bold(true)
		}
		return st.Render(string(r))
	})
	lines := append(sky, "")
	for _, l := range word {
		lines = append(lines, " "+l)
	}
	return strings.Join(lines, "\n")
}
