package host

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	sshconfig "github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/abhijitkrm/cometcli/internal/config"
)

// TrustHost, when set, is asked about a host key that isn't in
// known_hosts yet (trust on first use); a yes is saved to known_hosts.
// Unset — the TUI, watch, CI — an unknown host is an error.
var TrustHost func(host, fingerprint string) bool

// Passphrase, when set, is asked for a passphrase-protected key file.
// Unset, such keys are only usable through ssh-agent.
var Passphrase func(keyFile string) ([]byte, error)

// SSHConfigFile is the OpenSSH client config read for host aliases.
var SSHConfigFile = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "config")
}

// Target is an SSH destination after ~/.ssh/config is applied: the
// profile's own settings win, as ssh's -l/-p/-i/-J do.
type Target struct {
	Alias      string // what the profile names (host or ~/.ssh/config alias)
	HostName   string
	User       string
	Port       int
	Keys       []string // identity files to try, in order
	OnlyKeys   bool     // IdentitiesOnly: don't offer every agent key
	Jumps      []string // ProxyJump hops, nearest first
	KnownHosts []string
}

// Addr is host:port to dial.
func (t Target) Addr() string { return net.JoinHostPort(t.HostName, strconv.Itoa(t.Port)) }

func (t Target) String() string {
	s := t.User + "@" + t.HostName
	if t.Port != 22 {
		s += ":" + strconv.Itoa(t.Port)
	}
	return s
}

// Resolve applies ~/.ssh/config to a profile's transport.
func Resolve(t config.Transport) Target {
	tg := resolveSpec(t.Host, t.User, t.Port)
	if t.KeyFile != "" {
		tg.Keys = append([]string{expandTilde(t.KeyFile)}, tg.Keys...)
		tg.OnlyKeys = true
	}
	if t.Jump != "" {
		tg.Jumps = splitJumps(t.Jump)
	}
	return tg
}

// resolveSpec resolves a host or alias (optionally user@host:port).
func resolveSpec(spec, user string, port int) Target {
	if u, rest, ok := strings.Cut(spec, "@"); ok && user == "" {
		user, spec = u, rest
	}
	if h, p, err := net.SplitHostPort(spec); err == nil && port == 0 {
		spec = h
		port, _ = strconv.Atoi(p)
	}
	cfg := loadSSHConfig()
	get := func(key string) string { return cfgGet(cfg, spec, key) }
	tg := Target{Alias: spec, HostName: spec, User: user, Port: port}
	if h := get("HostName"); h != "" {
		tg.HostName = strings.ReplaceAll(h, "%h", spec)
	}
	if tg.User == "" {
		tg.User = get("User")
	}
	if tg.User == "" {
		tg.User = os.Getenv("USER")
	}
	if tg.Port == 0 {
		tg.Port, _ = strconv.Atoi(get("Port"))
	}
	if tg.Port == 0 {
		tg.Port = 22
	}
	for _, k := range cfgGetAll(cfg, spec, "IdentityFile") {
		k = strings.NewReplacer("%h", tg.HostName, "%r", tg.User, "%u", os.Getenv("USER")).Replace(k)
		tg.Keys = append(tg.Keys, expandTilde(k))
	}
	tg.OnlyKeys = strings.EqualFold(get("IdentitiesOnly"), "yes")
	if j := get("ProxyJump"); j != "" && !strings.EqualFold(j, "none") {
		tg.Jumps = splitJumps(j)
	}
	if f := get("UserKnownHostsFile"); f != "" {
		for _, k := range strings.Fields(f) {
			tg.KnownHosts = append(tg.KnownHosts, expandTilde(k))
		}
	} else {
		home, _ := os.UserHomeDir()
		tg.KnownHosts = []string{filepath.Join(home, ".ssh", "known_hosts")}
	}
	return tg
}

func splitJumps(s string) []string {
	var out []string
	for _, j := range strings.Split(s, ",") {
		if j = strings.TrimSpace(j); j != "" {
			out = append(out, j)
		}
	}
	return out
}

func loadSSHConfig() *sshconfig.Config {
	f, err := os.Open(SSHConfigFile())
	if err != nil {
		return nil
	}
	defer f.Close()
	cfg, err := sshconfig.Decode(f)
	if err != nil {
		return nil
	}
	return cfg
}

// cfgGet reads one value; the parser panics on Match blocks, which we
// treat as "not set" rather than crash.
func cfgGet(cfg *sshconfig.Config, alias, key string) (v string) {
	if cfg == nil {
		return ""
	}
	defer func() { _ = recover() }()
	v, _ = cfg.Get(alias, key)
	return v
}

func cfgGetAll(cfg *sshconfig.Config, alias, key string) (v []string) {
	if cfg == nil {
		return nil
	}
	defer func() { _ = recover() }()
	v, _ = cfg.GetAll(alias, key)
	return v
}

func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// --- authentication ----------------------------------------------------------

// signers collects keys to offer: the target's key files first, then
// ssh-agent's (all of them, or only the matching ones with IdentitiesOnly).
func signers(t Target) ([]ssh.Signer, error) {
	keys := t.Keys
	if len(keys) == 0 {
		home, _ := os.UserHomeDir()
		for _, k := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			keys = append(keys, filepath.Join(home, ".ssh", k))
		}
	}
	var out []ssh.Signer
	var locked []string
	var problems []string
	for _, k := range keys {
		raw, err := os.ReadFile(k)
		if err != nil {
			if len(t.Keys) > 0 && !os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("%s: %v", k, err))
			}
			continue
		}
		s, err := ssh.ParsePrivateKey(raw)
		var pm *ssh.PassphraseMissingError
		if errors.As(err, &pm) {
			if Passphrase != nil {
				var pw []byte
				if pw, err = Passphrase(k); err == nil {
					s, err = ssh.ParsePrivateKeyWithPassphrase(raw, pw)
				}
			}
			if err != nil {
				locked = append(locked, k)
				continue
			}
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", k, err))
			continue
		}
		out = append(out, s)
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			if as, err := agent.NewClient(conn).Signers(); err == nil {
				want := map[string]bool{}
				if t.OnlyKeys {
					for _, k := range keys {
						if pub, err := os.ReadFile(k + ".pub"); err == nil {
							if pk, _, _, _, err := ssh.ParseAuthorizedKey(pub); err == nil {
								want[string(pk.Marshal())] = true
							}
						}
					}
					for _, s := range out {
						want[string(s.PublicKey().Marshal())] = true
					}
				}
				for _, s := range as {
					if !t.OnlyKeys || want[string(s.PublicKey().Marshal())] {
						out = append(out, s)
					}
				}
			}
		}
	}
	if len(out) == 0 {
		switch {
		case len(locked) > 0:
			return nil, fmt.Errorf("ssh key %s is passphrase-protected — load it into ssh-agent first: ssh-add %s", locked[0], locked[0])
		case len(problems) > 0:
			return nil, fmt.Errorf("no usable ssh key: %s", strings.Join(problems, "; "))
		}
		return nil, fmt.Errorf("no ssh key for %s: set transport.key_file (cometcli profile add … --ssh-key), add IdentityFile for it in ~/.ssh/config, or load a key into ssh-agent (ssh-add)", t.Alias)
	}
	return out, nil
}

// --- host keys ---------------------------------------------------------------

// UnknownHostError is a host whose key isn't in known_hosts.
type UnknownHostError struct {
	Host        string
	Fingerprint string
}

func (e *UnknownHostError) Error() string {
	return fmt.Sprintf("ssh: %s isn't a known host (key %s) — check and trust it with `cometcli ssh test`, or connect once with ssh", e.Host, e.Fingerprint)
}

// ChangedHostKeyError is a host whose key differs from known_hosts.
type ChangedHostKeyError struct {
	Host, Fingerprint, Known string
}

func (e *ChangedHostKeyError) Error() string {
	return fmt.Sprintf("ssh: the host key of %s CHANGED (now %s; known_hosts has it at %s). "+
		"This can be a man-in-the-middle — cometcli won't connect. If the server was rebuilt, "+
		"verify the new key out of band, then remove the old entry: ssh-keygen -R %s",
		e.Host, e.Fingerprint, e.Known, knownhosts.Normalize(e.Host))
}

// hostKeys checks server keys against the target's known_hosts files and
// returns the key algorithms already known for addr — so a host known by
// its ECDSA key isn't negotiated to ed25519 and taken for a changed key.
func hostKeys(t Target, addr string) (ssh.HostKeyCallback, []string, error) {
	var files []string
	for _, f := range t.KnownHosts {
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
		}
	}
	db, err := knownhosts.New(files...)
	if err != nil {
		return nil, nil, fmt.Errorf("reading known_hosts: %w", err)
	}
	var algos []string
	if probe, err := randomKey(); err == nil {
		var ke *knownhosts.KeyError
		if errors.As(db(addr, &net.TCPAddr{IP: net.IPv4zero, Port: 22}, probe), &ke) {
			seen := map[string]bool{}
			for _, w := range ke.Want {
				for _, a := range algosFor(w.Key.Type()) {
					if !seen[a] {
						seen[a] = true
						algos = append(algos, a)
					}
				}
			}
		}
	}
	cb := func(host string, remote net.Addr, key ssh.PublicKey) error {
		err := db(host, remote, key)
		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) {
			return err
		}
		fp := ssh.FingerprintSHA256(key)
		if len(ke.Want) > 0 {
			w := ke.Want[0]
			return &ChangedHostKeyError{Host: host, Fingerprint: fp, Known: fmt.Sprintf("%s:%d", w.Filename, w.Line)}
		}
		if TrustHost == nil || !TrustHost(host, key.Type()+" "+fp) {
			return &UnknownHostError{Host: host, Fingerprint: fp}
		}
		return remember(t.KnownHosts[0], host, key)
	}
	return cb, algos, nil
}

func algosFor(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

func randomKey() (ssh.PublicKey, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewPublicKey(pub)
}

// remember appends a trusted host key to known_hosts.
func remember(file, host string, key ssh.PublicKey) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(host)}, key))
	return err
}

// --- connection --------------------------------------------------------------

// SSH executes on a remote machine over one long-lived connection, kept
// alive and redialed once if it drops (instance reboot, NAT timeout).
type SSH struct {
	transport config.Transport
	target    Target

	mu     sync.Mutex
	client *ssh.Client
	hops   []*ssh.Client // jump hosts, closed with the client
	stop   chan struct{}
	closed bool
}

func (s *SSH) String() string { return "ssh://" + s.target.String() }

// Target is where this connection goes, after ~/.ssh/config.
func (s *SSH) Target() Target { return s.target }

func dialSSH(ctx context.Context, t config.Transport) (*SSH, error) {
	if t.Host == "" {
		return nil, fmt.Errorf("ssh transport has no host — set transport.host (an address or a ~/.ssh/config alias)")
	}
	s := &SSH{transport: t, target: Resolve(t)}
	if err := s.connect(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SSH) connect(ctx context.Context) error {
	var hops []*ssh.Client
	var via *ssh.Client
	closeAll := func() {
		for i := len(hops) - 1; i >= 0; i-- {
			hops[i].Close()
		}
	}
	for _, j := range s.target.Jumps {
		c, err := dialHop(ctx, via, resolveSpec(j, "", 0))
		if err != nil {
			closeAll()
			return fmt.Errorf("ssh jump host %s: %w", j, err)
		}
		hops = append(hops, c)
		via = c
	}
	c, err := dialHop(ctx, via, s.target)
	if err != nil {
		closeAll()
		return fmt.Errorf("ssh %s: %w", s.target, err)
	}
	stop := make(chan struct{})
	s.mu.Lock()
	s.client, s.hops, s.stop = c, hops, stop
	s.mu.Unlock()
	go keepalive(c, stop)
	return nil
}

func dialHop(ctx context.Context, via *ssh.Client, t Target) (*ssh.Client, error) {
	auth, err := signers(t)
	if err != nil {
		return nil, err
	}
	cb, algos, err := hostKeys(t, t.Addr())
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:              t.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(auth...)},
		HostKeyCallback:   cb,
		HostKeyAlgorithms: algos,
		Timeout:           15 * time.Second,
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var conn net.Conn
	if via == nil {
		conn, err = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext(dctx, "tcp", t.Addr())
	} else {
		conn, err = via.DialContext(dctx, "tcp", t.Addr())
	}
	if err != nil {
		return nil, err
	}
	// the handshake has no context: bound it by the deadline instead
	if dl, ok := dctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, t.Addr(), cfg)
	if err != nil {
		conn.Close()
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("%s rejected the key(s) offered for user %q — check the user (ec2-user, ubuntu, …) and transport.key_file: %w", t.Addr(), t.User, err)
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(cc, chans, reqs), nil
}

// keepalive pings the server so idle NATs don't drop the connection, and
// closes it when the server stops answering (the next use redials).
func keepalive(c *ssh.Client, stop chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		res := make(chan error, 1)
		go func() {
			_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
			res <- err
		}()
		select {
		case <-stop:
			return
		case err := <-res:
			if err != nil {
				c.Close()
				return
			}
		case <-time.After(15 * time.Second):
			c.Close()
			return
		}
	}
}

func (s *SSH) current() (*ssh.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.client == nil {
		return nil, fmt.Errorf("ssh %s: connection closed", s.target)
	}
	return s.client, nil
}

// redial replaces a dead connection (unless another caller already did).
func (s *SSH) redial(ctx context.Context, dead *ssh.Client) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("ssh %s: connection closed", s.target)
	}
	if s.client != dead {
		s.mu.Unlock()
		return nil
	}
	s.shutdownLocked()
	s.mu.Unlock()
	return s.connect(ctx)
}

func (s *SSH) shutdownLocked() {
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
	for i := len(s.hops) - 1; i >= 0; i-- {
		s.hops[i].Close()
	}
	s.hops = nil
}

// session opens a channel, reconnecting once if the connection died.
func (s *SSH) session(ctx context.Context) (*ssh.Session, error) {
	c, err := s.current()
	if err != nil {
		return nil, err
	}
	sess, err := c.NewSession()
	if err == nil {
		return sess, nil
	}
	if rerr := s.redial(ctx, c); rerr != nil {
		return nil, fmt.Errorf("ssh connection lost (%v); reconnecting failed: %w", err, rerr)
	}
	if c, err = s.current(); err != nil {
		return nil, err
	}
	return c.NewSession()
}

// DialContext opens a TCP connection from the remote host — how cometcli
// reaches a node's RPC/gRPC ports that only listen on its localhost.
func (s *SSH) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := s.current()
	if err != nil {
		return nil, err
	}
	conn, err := c.DialContext(ctx, network, addr)
	var oce *ssh.OpenChannelError
	if err == nil || errors.As(err, &oce) || ctx.Err() != nil {
		// a refused port on the remote side isn't a dead connection
		return conn, err
	}
	if rerr := s.redial(ctx, c); rerr != nil {
		return nil, err
	}
	if c, err = s.current(); err != nil {
		return nil, err
	}
	return c.DialContext(ctx, network, addr)
}

func (s *SSH) Run(ctx context.Context, cmd string) (string, int, error) {
	return s.runStdin(ctx, cmd, nil)
}

func (s *SSH) runStdin(ctx context.Context, cmd string, stdin *bytes.Reader) (string, int, error) {
	sess, err := s.session(ctx)
	if err != nil {
		return "", -1, err
	}
	defer sess.Close()
	var out, serr bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &serr
	if stdin != nil {
		sess.Stdin = stdin
	}

	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return out.String(), -1, ctx.Err()
	case err := <-done:
		code := 0
		if ee, ok := err.(*ssh.ExitError); ok {
			code = ee.ExitStatus()
		}
		if err != nil && code == 0 {
			return out.String(), code, err
		}
		if code != 0 {
			return out.String(), code, fmt.Errorf("exit %d: %s", code, strings.TrimSpace(serr.String()))
		}
		return out.String(), code, nil
	}
}

// rpath quotes a remote path for the shell, keeping a leading ~ as the
// remote user's home.
func rpath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(p, "~/") {
		return `"$HOME"/` + shellQuote(p[2:])
	}
	return shellQuote(p)
}

func (s *SSH) ReadFile(ctx context.Context, path string) ([]byte, error) {
	out, code, err := s.Run(ctx, "cat -- "+rpath(path))
	if err != nil {
		return nil, fmt.Errorf("read %s (exit %d): %w", path, code, err)
	}
	return []byte(out), nil
}

func (s *SSH) WriteFile(ctx context.Context, path string, data []byte, perm os.FileMode) error {
	tmp := rpath(path + ".cometcli-tmp")
	cmd := fmt.Sprintf("umask 077 && cat > %s && chmod %o %s && mv %s %s",
		tmp, uint32(perm), tmp, tmp, rpath(path))
	_, code, err := s.runStdin(ctx, cmd, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("write %s (exit %d): %w", path, code, err)
	}
	return nil
}

func (s *SSH) Stat(ctx context.Context, path string) (os.FileInfo, error) {
	out, code, err := s.Run(ctx, "stat -c '%s %a %U %G %n' -- "+rpath(path))
	if err != nil {
		return nil, fmt.Errorf("stat %s (exit %d): %w", path, code, err)
	}
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) < 5 {
		return nil, fmt.Errorf("bad stat output for %s", path)
	}
	var size int64
	fmt.Sscan(f[0], &size)
	var mode uint64
	fmt.Sscanf(f[1], "%o", &mode)
	return &fileInfo{name: f[4], size: size, mode: os.FileMode(mode)}, nil
}

// Close shuts the connection. Safe on a nil or never-connected *SSH (a
// failed dial can surface as a typed nil inside the Host interface).
func (s *SSH) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.shutdownLocked()
	return nil
}

// --- reaching the node's ports -----------------------------------------------

// DialFunc opens a network connection.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Tunneler reaches addresses as the remote host sees them.
type Tunneler interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// OnNode reports whether endpoint (URL or host:port) points at the node
// machine itself: loopback, or the SSH host by name or address. Validators
// rarely expose RPC/gRPC beyond localhost, so such endpoints are reached
// through the SSH connection.
func OnNode(t config.Transport, endpoint string) bool {
	if t.Type != "ssh" {
		return false
	}
	h := endpointHost(endpoint)
	if h == "" {
		return false
	}
	if h == "localhost" || h == "0.0.0.0" || h == "::" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	tg := Resolve(t)
	return strings.EqualFold(h, tg.HostName) || strings.EqualFold(h, tg.Alias)
}

// NodeDialer dials through h, sending endpoints that name the SSH host to
// the node's localhost (its public address often isn't on an interface).
func NodeDialer(h Host, t config.Transport) DialFunc {
	tn, ok := h.(Tunneler)
	if !ok {
		return nil
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil {
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
		}
		conn, err := tn.DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("%s on %s (through ssh): %w", addr, t.Host, err)
		}
		return conn, nil
	}
}

// endpointHost extracts the host from "tcp://h:p", "http://h:p/x", "h:p;tls".
func endpointHost(ep string) string {
	ep = strings.TrimSuffix(ep, ";tls")
	if _, rest, ok := strings.Cut(ep, "://"); ok {
		ep = rest
	}
	ep, _, _ = strings.Cut(ep, "/")
	if h, _, err := net.SplitHostPort(ep); err == nil {
		return h
	}
	return ep
}
