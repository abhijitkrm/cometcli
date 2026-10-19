package cli

import (
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
)

func TestSignsBlocks(t *testing.T) {
	for _, tc := range []struct {
		role, valoper string
		want          bool
	}{
		{"validator", "", true},
		{"", "", true}, // role unset: could be a validator
		{"archive", "", false},
		{"rpc", "", false},
		{"sentry", "", false},
		{"archive", "cosmosvaloper1x", true}, // saved with a valoper
	} {
		p := &config.Profile{Role: tc.role, Metadata: map[string]string{}}
		if tc.valoper != "" {
			p.Metadata["valoper"] = tc.valoper
		}
		if got := signsBlocks(p); got != tc.want {
			t.Errorf("role %q valoper %q: got %v, want %v", tc.role, tc.valoper, got, tc.want)
		}
	}
}

func TestDoctorIcon(t *testing.T) {
	for _, tc := range []struct {
		ch   Check
		want string
	}{
		{Check{OK: true}, "✓"},
		{Check{OK: false, Detail: "0 peers"}, "✗"},
		{Check{Warn: true, Detail: "skipped: no route"}, "!"}, // a warning with detail is not a failure
		{Check{Warn: true}, "!"},
	} {
		if got := icon(tc.ch); got != tc.want {
			t.Errorf("%+v: got %s, want %s", tc.ch, got, tc.want)
		}
	}
}
