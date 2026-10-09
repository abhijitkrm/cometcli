package toolkit

import (
	"context"
	"testing"
)

// A password the operator put in the environment wins over asking: the
// CLI's terminal prompt fails in scripts and watch.
func TestSecretPrefersTheEnvironment(t *testing.T) {
	asked := false
	c := &Context{Context: context.Background(), Secret: func(*Context, string) (string, error) { asked = true; return "typed", nil }}
	t.Setenv("COMETCLI_CONTAINER_KEYRING_PASSWORD", "")
	if pw, _ := c.secret("pw"); pw != "typed" || !asked {
		t.Fatalf("without the env var the front-end asks: %q", pw)
	}
	asked = false
	t.Setenv("COMETCLI_CONTAINER_KEYRING_PASSWORD", "from-env")
	if pw, _ := c.secret("pw"); pw != "from-env" || asked {
		t.Fatalf("the env var wins: %q asked=%v", pw, asked)
	}
}
