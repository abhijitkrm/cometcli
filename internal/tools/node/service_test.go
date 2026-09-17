package node

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// recHost records commands instead of executing them.
type recHost struct {
	cmds  []string
	reply string
	code  int
	err   error
}

func (r *recHost) String() string { return "rec" }
func (r *recHost) Run(ctx context.Context, cmd string) (string, int, error) {
	r.cmds = append(r.cmds, cmd)
	return r.reply, r.code, r.err
}
func (r *recHost) ReadFile(ctx context.Context, p string) ([]byte, error) { return nil, nil }
func (r *recHost) WriteFile(ctx context.Context, p string, d []byte, m os.FileMode) error {
	return nil
}
func (r *recHost) Stat(ctx context.Context, p string) (os.FileInfo, error) { return nil, nil }

func svcCtx(h *recHost, svcType, unit string) *toolkit.Context {
	return &toolkit.Context{
		Context: context.Background(),
		Profile: &config.Profile{
			Name:    "t",
			Service: config.Service{Type: svcType, Unit: unit},
		},
		AutoApproveBelow: toolkit.TierOnChain, // local-change auto-approves
	}
}

func TestServiceCommands(t *testing.T) {
	cases := []struct {
		typ, action, want string
	}{
		{"systemd", "status", "systemctl status 'evmd.service'"},
		{"systemd", "restart", "systemctl restart 'evmd.service'"},
		{"docker", "status", "docker ps -a --filter name='evmd'"},
		{"docker", "stop", "docker stop 'evmd'"},
		{"launchd", "status", "launchctl list | grep -i 'evmd'"},
		{"launchd", "restart", "launchctl kickstart -k gui/$(id -u)/'evmd'"},
	}
	for _, tc := range cases {
		h := &recHost{reply: "ok"}
		c := svcCtx(h, tc.typ, "evmd")
		if tc.typ == "systemd" {
			c.Profile.Service.Unit = "evmd.service"
		}
		c.SetHost(h)
		if _, _, err := svcRun(c, h, tc.action); err != nil {
			t.Fatalf("%s %s: %v", tc.typ, tc.action, err)
		}
		if len(h.cmds) != 1 || !strings.Contains(h.cmds[0], tc.want) {
			t.Fatalf("%s %s → cmd %v, want substring %q", tc.typ, tc.action, h.cmds, tc.want)
		}
	}
}

func TestServiceErrorSurfaced(t *testing.T) {
	h := &recHost{reply: "Unit evmd.service could not be found.", code: 4, err: fmt.Errorf("exit 4")}
	c := svcCtx(h, "systemd", "evmd.service")
	out, code, err := svcRun(c, h, "status")
	if code != 4 || err == nil || !strings.Contains(out, "could not be found") {
		t.Fatalf("inactive service not surfaced: out=%q code=%d err=%v", out, code, err)
	}
}

func TestServiceApprovalGate(t *testing.T) {
	// status needs no approval; restart does (local-change tier)
	h := &recHost{reply: "ok"}
	c := svcCtx(h, "systemd", "evmd.service")
	c.AutoApproveBelow = toolkit.TierLocalChange // local-change NOT auto
	approved := false
	c.Approver = func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) {
		approved = true
		return true, nil
	}
	c.SetHost(h)
	st := serviceTool{}
	if _, err := st.Run(c, map[string]any{"action": "status"}); err != nil {
		t.Fatal(err)
	}
	if approved {
		t.Fatal("status should not prompt")
	}
	if _, err := st.Run(c, map[string]any{"action": "restart"}); err != nil {
		t.Fatal(err)
	}
	if !approved {
		t.Fatal("restart must prompt")
	}
}
