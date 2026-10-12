package networktool

import (
	"strings"
	"testing"
)

func TestHostPreparePlan(t *testing.T) {
	ready := hostState{"os": "ubuntu", "pkg": "apt", "user": "ubuntu", "docker": "yes", "daemon": "ok", "compose": "yes", "git": "yes", "jq": "yes", "curl": "yes", "group": "yes"}
	what, _, err := ready.plan()
	if err != nil || len(what) != 0 {
		t.Fatalf("a ready host needs nothing: %v %v", what, err)
	}
	fresh := hostState{"os": "ubuntu", "pkg": "apt", "user": "ubuntu", "docker": "no", "daemon": "down", "compose": "no", "git": "yes", "jq": "no", "curl": "yes", "group": "no"}
	what, steps, _ := fresh.plan()
	got := strings.Join(what, " | ")
	for _, want := range []string{"install jq", "install docker (docker.com repository)", "start docker", "docker group"} {
		if !strings.Contains(got, want) {
			t.Errorf("fresh ubuntu: missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "compose plugin v") {
		t.Errorf("docker-ce brings compose on ubuntu: %s", got)
	}
	if !strings.Contains(strings.Join(steps, "\n"), "download.docker.com/linux/$ID") {
		t.Error("docker repo step missing")
	}
	// Amazon Linux: docker package, no compose package
	al := hostState{"os": "amzn", "pkg": "dnf", "arch": "x86_64", "user": "ec2-user", "docker": "no", "daemon": "down", "compose": "no", "git": "no", "jq": "yes", "curl": "yes", "group": "no"}
	what, steps, _ = al.plan()
	got = strings.Join(what, " | ")
	if !strings.Contains(got, "Amazon Linux package") || !strings.Contains(got, "compose plugin "+composeVersion) || !strings.Contains(strings.Join(steps, "\n"), "docker-compose-linux-x86_64") {
		t.Errorf("amazon linux: %s", got)
	}
	// running as root-only: the group, nothing else
	what, _, _ = hostState{"os": "amzn", "pkg": "dnf", "user": "ec2-user", "docker": "yes", "daemon": "root-only", "compose": "yes", "git": "yes", "jq": "yes", "curl": "yes", "group": "no"}.plan()
	if len(what) != 1 || !strings.Contains(what[0], "docker group") {
		t.Errorf("root-only docker: %v", what)
	}
	if _, _, err := (hostState{"os": "alpine", "pkg": "none", "docker": "no", "git": "yes", "jq": "yes", "curl": "yes"}).plan(); err == nil {
		t.Error("an unknown OS must say so")
	}
}
