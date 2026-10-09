package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"path"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
)

func sshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "SSH transport: check a remote node's connection",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "test [profile]",
		Short: "Connect to a profile's node over ssh, trust its host key, and check access",
		Long: `Resolves the profile's ssh target through ~/.ssh/config, connects (asking
to trust a new host key, as ssh does), and checks what cometcli needs on
the node: the service manager, logs, the node home, and the node's
RPC/gRPC/JSON-RPC ports, reached through the ssh connection when they
only listen on the node's localhost.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			name := flagProfile
			if len(a) == 1 {
				name = a[0]
			}
			p, err := cfg.ActiveProfile(name)
			if err != nil {
				return err
			}
			return sshTest(cmd.Context(), p, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	})
	return cmd
}

func sshTest(ctx context.Context, p *config.Profile, in io.Reader, out io.Writer) error {
	if p.Transport.Type != "ssh" {
		return fmt.Errorf("profile %s uses the %s transport — set one up with: cometcli profile add %s --ssh-host <host or ~/.ssh/config alias>", p.Name, orDash(p.Transport.Type), p.Name)
	}
	say := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	tg := host.Resolve(p.Transport)
	say("%s → %s", p.Name, tg)
	if tg.Alias != tg.HostName {
		say("  ~/.ssh/config alias %s", tg.Alias)
	}
	if len(tg.Jumps) > 0 {
		say("  via %s", strings.Join(tg.Jumps, " → "))
	}
	if len(tg.Keys) > 0 {
		say("  key %s", strings.Join(tg.Keys, ", "))
	} else {
		say("  key: ssh-agent and ~/.ssh/id_*")
	}

	restore := interactiveSSH(bufio.NewReader(in), out)
	h, err := host.Connect(ctx, p)
	restore()
	if err != nil {
		return err
	}
	if c, ok := h.(io.Closer); ok {
		defer c.Close()
	}
	who, _, _ := run(ctx, h, `echo "$(id -un)@$(hostname) · $(uname -sr)"`)
	say("✓ connected, host key verified: %s", strings.TrimSpace(who))

	check := func(ok bool, label, detail string) {
		mark := "✓"
		if !ok {
			mark = "!"
		}
		if detail != "" {
			label += " — " + detail
		}
		say("%s %s", mark, label)
	}
	firstLine := func(s string) string { return strings.SplitN(strings.TrimSpace(s), "\n", 2)[0] }

	switch p.Service.Type {
	case "docker":
		o, code, _ := run(ctx, h, "docker inspect -f '{{.State.Status}}' "+shq(p.Service.Unit)+" 2>&1")
		switch {
		case code == 0:
			check(true, "docker: container "+p.Service.Unit, strings.TrimSpace(o))
		case strings.Contains(o, "permission denied"):
			check(false, "docker: no access to the docker socket", "add the ssh user to the docker group: sudo usermod -aG docker $USER, then reconnect")
		default:
			check(false, "docker: container "+p.Service.Unit, firstLine(o))
		}
	case "systemd":
		o, _, _ := run(ctx, h, "systemctl is-active "+shq(p.Service.Unit)+" 2>&1")
		check(strings.TrimSpace(o) == "active", "systemd: "+p.Service.Unit, firstLine(o))
		o, code, _ := run(ctx, h, "journalctl -u "+shq(p.Service.Unit)+" -n 1 --no-pager 2>&1")
		switch {
		case code == 0 && !strings.Contains(o, "No journal files") && !strings.Contains(o, "not seeing messages"):
			check(true, "logs: journalctl readable", "")
		default:
			check(false, "logs: journalctl", "the ssh user may need the systemd-journal or adm group: sudo usermod -aG systemd-journal $USER")
		}
		if _, code, _ := run(ctx, h, "sudo -n true 2>&1"); code != 0 {
			check(false, "sudo: needs a password", "restart/stop will fail unless the ssh user can run systemctl through sudo without a password")
		} else {
			check(true, "sudo: passwordless", "")
		}
	}
	if p.Home != "" {
		cfgFile := path.Join(p.Home, "config", "config.toml")
		if p.Service.Type == "docker" {
			check(true, "node home "+p.Home, "on the host (mounted into the container)")
		}
		_, err := h.ReadFile(ctx, cfgFile)
		check(err == nil, "node config "+cfgFile, errDetail(err))
	}

	for _, ep := range []struct{ name, url string }{
		{"CometBFT RPC", p.Endpoints.Comet},
		{"gRPC", p.Endpoints.GRPC},
		{"EVM JSON-RPC", p.Endpoints.EVM},
	} {
		if ep.url == "" {
			continue
		}
		hostport := hostPort(ep.url)
		dial := (&net.Dialer{}).DialContext
		how := "direct"
		if host.OnNode(p.Transport, ep.url) {
			dial, how = host.NodeDialer(h, p.Transport), "through ssh"
		}
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := dial(dctx, "tcp", hostport)
		cancel()
		if err == nil {
			conn.Close()
		}
		check(err == nil, fmt.Sprintf("%s %s (%s)", ep.name, hostport, how), errDetail(err))
	}
	return nil
}

func errDetail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// hostPort turns an endpoint URL into host:port, with the scheme's port
// when it has none.
func hostPort(ep string) string {
	ep = strings.TrimSuffix(ep, ";tls")
	scheme := ""
	if s, rest, ok := strings.Cut(ep, "://"); ok {
		scheme, ep = s, rest
	}
	ep, _, _ = strings.Cut(ep, "/")
	if _, _, err := net.SplitHostPort(ep); err == nil {
		return ep
	}
	if scheme == "https" {
		return net.JoinHostPort(ep, "443")
	}
	return net.JoinHostPort(ep, "80")
}
