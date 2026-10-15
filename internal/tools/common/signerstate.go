package common

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// SignerStateCorrupt reports whether a validator's
// data/priv_validator_state.json exists but doesn't parse. A missing file
// is not corrupt (a fresh node); a file that can't be read is not judged.
func SignerStateCorrupt(c *toolkit.Context, h host.Host) (bool, string) {
	if c.Profile == nil || !c.Profile.IsValidator() {
		return false, ""
	}
	raw, at, err := ReadNodeFile(c, h, "data/priv_validator_state.json")
	if err != nil {
		return false, ""
	}
	if why := ParseSignerState(raw); why != "" {
		return true, fmt.Sprintf("%s is corrupt (%s)", at, why)
	}
	return false, ""
}

// ParseSignerState returns why a priv_validator_state.json is unusable, or
// "" when it parses with a numeric height.
func ParseSignerState(raw []byte) string {
	var pvs struct {
		Height json.RawMessage `json:"height"`
	}
	if err := json.Unmarshal(raw, &pvs); err != nil {
		return fmt.Sprintf("%d bytes, not JSON: %v", len(raw), err)
	}
	if _, err := strconv.ParseInt(strings.Trim(string(pvs.Height), `"`), 10, 64); err != nil {
		return fmt.Sprintf("height %s isn't a number", OneLine(string(pvs.Height)))
	}
	return ""
}

// GuardSignerState refuses to start a validator on a corrupt signer state:
// it can't know the last height it signed, so it may sign twice.
func GuardSignerState(c *toolkit.Context, h host.Host, what string) error {
	if bad, why := SignerStateCorrupt(c, h); bad {
		return fmt.Errorf("refusing to %s %s: %s — the validator can't tell what it last signed and could double-sign. "+
			"Find the last height it signed (its logs or the chain), write a state file at or above it, then retry", what, c.Profile.Name, why)
	}
	return nil
}
