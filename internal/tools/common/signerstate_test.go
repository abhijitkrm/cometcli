package common

import "testing"

func TestParseSignerState(t *testing.T) {
	for raw, bad := range map[string]bool{
		`{"height":"12345","round":0,"step":3}`: false,
		`{"height":678,"round":0,"step":3}`:     false,
		"{\"height\":\"1\x00\x00":               true,
		``:                                      true,
		`{"height":"abc","round":0}`:            true,
		`{"round":0}`:                           true,
	} {
		if got := ParseSignerState([]byte(raw)) != ""; got != bad {
			t.Errorf("ParseSignerState(%q) corrupt=%v, want %v", raw, got, bad)
		}
	}
}
