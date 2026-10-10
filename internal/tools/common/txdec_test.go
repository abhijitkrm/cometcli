package common

import (
	"context"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// cosmos-sdk v0.54 decodes cosmos.Dec tx fields as plain decimals; older
// versions as the legacy scaled integer. 5% must be sent in the right form.
func TestTxDecFollowsTheSDK(t *testing.T) {
	for sdk, want := range map[string]string{"v0.53.4": "50000000000000000", "v0.54.3": "0.05", "v0.55.0": "0.05"} {
		c := &toolkit.Context{Context: context.Background(), Profile: &config.Profile{Metadata: map[string]string{"sdk_version": sdk}}}
		got, err := TxDec(c, "0.05")
		if err != nil || got != want {
			t.Errorf("sdk %s: TxDec(0.05) = %q %v, want %q", sdk, got, err, want)
		}
	}
	c := &toolkit.Context{Context: context.Background(), Profile: &config.Profile{Metadata: map[string]string{"sdk_version": "v0.54.3"}}}
	if _, err := TxDec(c, "five"); err == nil {
		t.Error("a non-decimal must be refused")
	}
}
