package toolkit

import (
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/audit"
)

// KeyUseMark marks an approval request as a key use (front-ends may treat
// it specially: remote approvals of key use expire sooner).
const KeyUseMark = "_key_use"

// KeyUse is one use of a signing key, shown to the operator before the key
// is touched.
type KeyUse struct {
	Address string // the account the key signs as
	Where   string // which key and where it lives (signer.Describe)
	Op      string // what it signs, e.g. "transaction"
	Doc     string // the decoded document: messages, fee, chain, sequence
	Purpose string // why, in the agent's or operator's words
}

func (k KeyUse) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "KEY ACCESS — sign a %s and broadcast it\n", k.Op)
	fmt.Fprintf(&b, "  account: %s\n", k.Address)
	fmt.Fprintf(&b, "  key:     %s — the key stays there; cometcli only gets the signature\n", k.Where)
	if k.Purpose != "" {
		fmt.Fprintf(&b, "  why:     %s\n", k.Purpose)
	}
	b.WriteString("  scope:   this one signature — the next use asks again\n")
	b.WriteString(k.Doc)
	return b.String()
}

// ApproveKeyUse asks the operator, every time, before a key signs. No
// permission rule, mode, autopilot or earlier approval covers a key use,
// and the decision is audited as its own event.
func ApproveKeyUse(c *Context, k KeyUse, detail map[string]any) error {
	if k.Purpose == "" {
		k.Purpose = c.Purpose
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail[KeyUseMark] = true
	delete(detail, "doc") // the prompt carries it; front-ends would show it twice
	prompt := k.String()
	name := ""
	if c.Profile != nil {
		name = c.Profile.Name
	}
	var ok bool
	var err error
	if c.Approver == nil {
		err = fmt.Errorf("signing needs the operator's approval and there's no one to ask — run it interactively")
	} else {
		ok, err = c.Approver(c, prompt, TierOnChain, detail)
	}
	if c.Audit != nil {
		_ = c.Audit.Log(audit.KindKey, name, map[string]any{
			"account": k.Address, "key": k.Where, "op": k.Op, "purpose": k.Purpose,
			"tool": c.ToolName, "approved": ok && err == nil,
		})
		c.Audit.Approval(name, strings.SplitN(prompt, "\n", 2)[0], ok && err == nil)
	}
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("denied: the operator didn't approve using the key for %s", k.Address)
	}
	return nil
}
