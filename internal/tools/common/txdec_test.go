package common

import (
	"testing"
)

// cosmos.Dec tx fields are gogoproto LegacyDec on every SDK version, v0.54
// included: the wire form is the scaled integer, and the input is always a
// fraction (1 is 100%, not an already-scaled 10^-18).
func TestTxDecIsAlwaysScaled(t *testing.T) {
	for in, want := range map[string]string{"0.05": "50000000000000000", "0.06": "60000000000000000", "1": "1000000000000000000", "0": "0"} {
		if got, err := TxDec(nil, in); err != nil || got != want {
			t.Errorf("TxDec(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"five", "1/20", "0.0000000000000000001"} {
		if _, err := TxDec(nil, bad); err == nil {
			t.Errorf("TxDec(%q) must be refused", bad)
		}
	}
}
