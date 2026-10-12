package networktool

import (
	"fmt"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// hostPrepareTool gets a fresh host ready to run nodes: docker, compose, git,
// jq — what build and provision need — installed and running.
type hostPrepareTool struct{}

func (hostPrepareTool) Name() string { return "host.prepare" }
func (hostPrepareTool) Desc() string {
	return "Get this profile's host ready to build and run nodes: installs what's missing of docker (enabled at boot), " +
		"the compose plugin, git, jq and curl (Ubuntu/Debian or Amazon Linux/RHEL, with passwordless sudo), and lets the " +
		"user run docker. Checks first and changes nothing that's already there; shows the plan and asks."
}
func (hostPrepareTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (hostPrepareTool) Tier() toolkit.Tier     { return toolkit.TierLocalChange }
func (hostPrepareTool) Timeout(toolkit.Args) time.Duration {
	return 20 * time.Minute
}

// composeVersion is the compose plugin installed where the OS has no
// package for it (Amazon Linux).
const composeVersion = "v2.29.7"

const probeScript = `. /etc/os-release 2>/dev/null
echo "os=${ID:-unknown}"; echo "like=${ID_LIKE:-}"; echo "arch=$(uname -m)"; echo "user=$(id -un)"
if command -v apt-get >/dev/null; then echo pkg=apt; elif command -v dnf >/dev/null; then echo pkg=dnf; elif command -v yum >/dev/null; then echo pkg=yum; else echo pkg=none; fi
sudo -n true 2>/dev/null && echo sudo=yes || echo sudo=no
command -v docker >/dev/null && echo docker=yes || echo docker=no
docker info >/dev/null 2>&1 && echo daemon=ok || { sudo -n docker info >/dev/null 2>&1 && echo daemon=root-only || echo daemon=down; }
docker compose version >/dev/null 2>&1 && echo compose=yes || echo compose=no
for t in git jq curl; do command -v $t >/dev/null && echo $t=yes || echo $t=no; done
id -nG | tr ' ' '\n' | grep -qx docker && echo group=yes || echo group=no
`

type hostState map[string]string

func probe(c *toolkit.Context, h host.Host) (hostState, error) {
	out, _, err := run(c, h, probeScript, 2*time.Minute)
	if err != nil && out == "" {
		return nil, err
	}
	st := hostState{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			st[k] = v
		}
	}
	return st, nil
}

// plan is what needs doing, as (description, script) pairs.
func (st hostState) plan() ([]string, []string, error) {
	var what, steps []string
	add := func(w, s string) { what, steps = append(what, w), append(steps, s) }
	pkg := st["pkg"]
	var missing []string
	for _, t := range []string{"git", "jq", "curl"} {
		if st[t] != "yes" {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		switch pkg {
		case "apt":
			add("install "+strings.Join(missing, ", "), "sudo -n apt-get update -qq && sudo -n DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "+strings.Join(missing, " "))
		case "dnf", "yum":
			add("install "+strings.Join(missing, ", "), "sudo -n "+pkg+" install -y -q "+strings.Join(missing, " "))
		default:
			return nil, nil, fmt.Errorf("no apt/dnf/yum on this host (%s) — install %s yourself", st["os"], strings.Join(missing, ", "))
		}
	}
	amzn := st["os"] == "amzn"
	if st["docker"] != "yes" {
		switch {
		case pkg == "apt":
			add("install docker (docker.com repository) with the compose and buildx plugins", `set -e
sudo -n apt-get update -qq && sudo -n DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates curl gnupg lsb-release
sudo -n install -m 0755 -d /etc/apt/keyrings
. /etc/os-release
curl -fsSL "https://download.docker.com/linux/$ID/gpg" | sudo -n gpg --dearmor --yes -o /etc/apt/keyrings/docker.gpg
sudo -n chmod a+r /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/$ID $(lsb_release -cs) stable" | sudo -n tee /etc/apt/sources.list.d/docker.list >/dev/null
sudo -n apt-get update -qq && sudo -n DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin`)
		case amzn:
			add("install docker (Amazon Linux package)", "sudo -n "+pkg+" install -y -q docker")
		case pkg == "dnf" || pkg == "yum":
			add("install docker (docker.com repository) with the compose and buildx plugins", `set -e
sudo -n `+pkg+` install -y -q yum-utils
sudo -n yum-config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
sudo -n `+pkg+` install -y -q docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin`)
		default:
			return nil, nil, fmt.Errorf("can't install docker automatically on %s — see https://docs.docker.com/engine/install/", st["os"])
		}
	}
	if st["daemon"] != "ok" && st["daemon"] != "root-only" || st["docker"] != "yes" {
		add("start docker and enable it at boot", "sudo -n systemctl enable --now docker")
	}
	if st["compose"] != "yes" && (st["docker"] == "yes" || amzn) {
		// Amazon Linux's docker has no compose plugin package
		arch := st["arch"]
		if arch == "arm64" {
			arch = "aarch64"
		}
		add("install the docker compose plugin "+composeVersion, fmt.Sprintf(`set -e
sudo -n mkdir -p /usr/local/lib/docker/cli-plugins
sudo -n curl -fsSL -o /usr/local/lib/docker/cli-plugins/docker-compose https://github.com/docker/compose/releases/download/%s/docker-compose-linux-%s
sudo -n chmod +x /usr/local/lib/docker/cli-plugins/docker-compose`, composeVersion, arch))
	}
	if st["group"] != "yes" {
		add("let "+st["user"]+" run docker (docker group)", "sudo -n usermod -aG docker "+shellQ(st["user"]))
	}
	return what, steps, nil
}

func shellQ(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (hostPrepareTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	if c.Profile == nil {
		return nil, fmt.Errorf("host.prepare needs a profile for the host (cometcli profile add <name> --ssh-host …)")
	}
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	st, err := probe(c, h)
	if err != nil {
		return nil, err
	}
	what, steps, err := st.plan()
	if err != nil {
		return nil, err
	}
	if len(what) == 0 {
		return &toolkit.Result{Text: fmt.Sprintf("%s is ready: docker running, compose, git, jq, curl; %s can run docker", c.Profile.Name, st["user"]),
			Data: map[string]any{"changed": false}}, nil
	}
	if st["sudo"] != "yes" {
		return nil, fmt.Errorf("%s needs root for: %s — and sudo asks for a password there. Run these on the host yourself:\n  %s",
			c.Profile.Name, strings.Join(what, "; "), strings.Join(steps, "\n  "))
	}
	if err := c.Approve(fmt.Sprintf("prepare %s (%s): %s", c.Profile.Name, st["os"], strings.Join(what, "; ")), toolkit.TierLocalChange,
		map[string]any{"steps": strings.Join(steps, "\n")}); err != nil {
		return nil, err
	}
	var b strings.Builder
	for i, s := range steps {
		if c.Progress != nil {
			c.Progress(what[i])
		}
		out, code, err := run(c, h, s, 15*time.Minute)
		if err != nil || code != 0 {
			return nil, fmt.Errorf("%s failed (exit %d): %v\n%s", what[i], code, err, lastLines(out, 8))
		}
		fmt.Fprintf(&b, "✓ %s\n", what[i])
	}
	// a new group takes effect on a new login: reconnect, then check
	// docker works as the user
	if nh, err := host.Connect(c, c.Profile); err == nil {
		if cl, ok := h.(interface{ Close() error }); ok {
			_ = cl.Close()
		}
		c.SetHost(nh)
		h = nh
	}
	after, _ := probe(c, h)
	if after["daemon"] != "ok" || after["compose"] != "yes" {
		return nil, fmt.Errorf("%s: installed, but docker still isn't usable as %s (daemon %s, compose %s) — log in again or check `systemctl status docker`",
			c.Profile.Name, after["user"], after["daemon"], after["compose"])
	}
	fmt.Fprintf(&b, "%s is ready: docker running, compose, git, jq, curl; %s can run docker", c.Profile.Name, after["user"])
	return &toolkit.Result{Text: b.String(), Data: map[string]any{"changed": true, "done": what}}, nil
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}
