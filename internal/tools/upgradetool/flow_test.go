package upgradetool

import (
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"sort"
	"testing"
)

func TestSemverOrder(t *testing.T) {
	tags := []string{"v0.7.3", "v0.6.10", "v0.6.2", "v0.7.0", "v0.6.1"}
	sort.Slice(tags, func(i, j int) bool { return semverLess(tags[i], tags[j]) })
	want := []string{"v0.6.1", "v0.6.2", "v0.6.10", "v0.7.0", "v0.7.3"}
	for i := range want {
		if tags[i] != want[i] {
			t.Fatalf("got %v", tags)
		}
	}
	if !semverLess("v0.6.0", "v0.6.1") || semverLess("v0.6.1", "v0.6.0") || semverLess("v0.6.1", "v0.6.1") {
		t.Fatal("pairwise order wrong")
	}
}

func TestHandlerNamesFromSource(t *testing.T) {
	src := `const UpgradeName = "v0.6.0-to-v0.7.0"
func (app EVMD) RegisterUpgradeHandlers() {
	app.UpgradeKeeper.SetUpgradeHandler("v0.6.0-to-v0.6.1", func() {})
	app.UpgradeKeeper.SetUpgradeHandler(
		UpgradeName,`
	var got []string
	for _, m := range upgradeNameRe.FindAllStringSubmatch(src, -1) {
		got = append(got, m[1])
	}
	for _, m := range setHandlerRe.FindAllStringSubmatch(src, -1) {
		got = append(got, m[1])
	}
	if len(got) != 2 || got[0] != "v0.6.0-to-v0.7.0" || got[1] != "v0.6.0-to-v0.6.1" {
		t.Fatalf("names = %v", got)
	}
}

func TestUpgradeNeededLine(t *testing.T) {
	m := upgradeNeededRe.FindStringSubmatch(`ERR UPGRADE "v0.6.0-to-v0.6.1" NEEDED at height: 18150000: module=x/upgrade`)
	if m == nil || m[1] != "v0.6.0-to-v0.6.1" || m[2] != "18150000" {
		t.Fatalf("%v", m)
	}
}

func TestImageNames(t *testing.T) {
	c := &toolkit.Context{Profile: &config.Profile{Metadata: map[string]string{"image": "primium-{tag}"}}}
	cases := []struct{ tmpl, running, want string }{
		{"", "primium-evm:v0.6.1", "primium-v0.7.2"},                 // the profile's naming scheme
		{"primium:{version}", "primium-evm:v0.6.1", "primium:0.7.2"}, // explicit wins
	}
	for _, x := range cases {
		if got := ImageFor(c, x.tmpl, x.running, "v0.7.2"); got != x.want {
			t.Errorf("ImageFor(%q) = %q, want %q", x.tmpl, got, x.want)
		}
	}
	if got := ImageFor(&toolkit.Context{Profile: &config.Profile{}}, "", "primium-evm:v0.6.1", "v0.7.2"); got != "primium-evm:v0.7.2" {
		t.Errorf("default = %q", got)
	}
}

func TestBreakingNotes(t *testing.T) {
	for note, want := range map[string]bool{
		"__This release is state breaking.__":         true,
		"Consensus-breaking change to the fee market": true,
		"This is a non-state-breaking patch release":  false,
		"Bug fixes only.":                             false,
	} {
		got := breakingRe.MatchString(note) && !notBreakingRe.MatchString(note)
		if got != want {
			t.Errorf("%q: breaking=%v, want %v", note, got, want)
		}
	}
	if m := goLineRe.FindStringSubmatch("module x\n\ngo 1.25.9\n"); m == nil || m[2] != "1.25.9" {
		t.Fatalf("go line: %v", m)
	}
	if m := fromGoRe.FindStringSubmatch("ARG X\nFROM golang:1.25.8-alpine AS b\n"); m == nil || m[1] != "1.25.8" {
		t.Fatalf("from line: %v", m)
	}
}
