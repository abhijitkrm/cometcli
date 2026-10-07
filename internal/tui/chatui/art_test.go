package chatui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestCometArtFitsAndDegrades(t *testing.T) {
	w := artWidth()
	if cometArt(w+1) != "" {
		t.Fatal("art drawn on a terminal narrower than it")
	}
	art := cometArt(w + 2)
	if art == "" {
		t.Fatal("no art on a wide terminal")
	}
	for _, l := range strings.Split(art, "\n") {
		if lw := lipgloss.Width(l); lw > w+1 {
			t.Fatalf("line wider (%d) than the art (%d): %q", lw, w, l)
		}
	}
	plain := ansiRe.ReplaceAllString(art, "")
	for _, want := range []string{"●", "███╗   ███╗", "╚══════╝"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("art lacks %q", want)
		}
	}
	// every wordmark row is the same width (the font is consistent)
	rows := wordmark("COMETCLI")
	for _, r := range rows[1:] {
		if len([]rune(r)) != len([]rune(rows[0])) {
			t.Fatalf("ragged wordmark: %q", r)
		}
	}
}
