package toolkit

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/keys"
	"github.com/abhijitkrm/cometcli/internal/tx"
)

// Chooser asks the operator to pick one of options (returns its index).
type Chooser func(c *Context, prompt string, options []string) (int, error)

// SecretFunc asks the operator for a secret (input hidden, never logged).
type SecretFunc func(c *Context, prompt string) (string, error)

// secret is COMETCLI_CONTAINER_KEYRING_PASSWORD when the operator set it
// (scripts, CI, watch), else asked via the front-end.
func (c *Context) secret(prompt string) (string, error) {
	if pw := os.Getenv("COMETCLI_CONTAINER_KEYRING_PASSWORD"); pw != "" {
		return pw, nil
	}
	if c.Secret != nil {
		return c.Secret(c, prompt)
	}
	return "", fmt.Errorf("%s: no terminal to ask on — run interactively or set COMETCLI_CONTAINER_KEYRING_PASSWORD", prompt)
}

// signerContainer is the container that holds the node's keyring.
func (c *Context) signerContainer() string {
	s := c.Profile.Signer
	if s.Container != "" {
		return s.Container
	}
	if c.Profile.Service.Type == "docker" {
		return c.Profile.Service.Unit
	}
	return ""
}

func (c *Context) containerKey() string {
	if k := c.Profile.Signer.ContainerKey; k != "" {
		return k
	}
	// evmd takes an address wherever it takes a key name: the operator
	// account finds the right key whatever it's called in the keyring
	if v := c.Profile.Metadata["valoper"]; v != "" {
		if acc, err := keys.AccFromValoper(v); err == nil {
			return acc
		}
	}
	return c.Profile.Signer.Key
}

// TxSigner returns a transaction builder for broadcasting, choosing the
// signer: signer.mode when set; otherwise the only one available; when
// both cometcli's keyring and the node container's keyring can sign, the
// operator is asked right now.
func (c *Context) TxSigner() (*tx.Builder, error) {
	if c.Profile == nil {
		return nil, errNoProfile
	}
	// one choice per context: the address a tool puts in its messages and
	// the key that signs them must be the same
	c.mu.Lock()
	cached := c.signerB
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	b, err := c.chooseSigner()
	if err == nil {
		c.mu.Lock()
		c.signerB = b
		c.mu.Unlock()
	}
	return b, err
}

// wrongAccount explains why a local key can't sign for this profile's
// validator ("" when it can, or there's no valoper to check against).
func (c *Context) wrongAccount(b *tx.Builder) string {
	if c.Profile.Metadata["valoper"] == "" {
		return ""
	}
	if want, err := keys.AccFromValoper(c.Profile.Metadata["valoper"]); err == nil && want != b.Address() {
		return fmt.Sprintf("cometcli keyring key %q is %s, not this validator's operator account %s", c.Profile.Signer.Key, b.Address(), want)
	}
	return ""
}

func (c *Context) chooseSigner() (*tx.Builder, error) {
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	var localB *tx.Builder
	var localErr error
	if c.Profile.Signer.Key != "" {
		localB, localErr = tx.NewBuilder(c.Context, g, c.Profile, c.Audit)
	} else {
		localErr = fmt.Errorf("no signer.key")
	}
	// a key that isn't this validator's operator account would sign a
	// valid tx as someone else (a shared keyring, a copied profile)
	if localErr == nil {
		if why := c.wrongAccount(localB); why != "" {
			localErr, localB = fmt.Errorf("%s", why), nil
		}
	}
	container, ckey := c.signerContainer(), c.containerKey()
	containerOK := container != "" && ckey != ""
	switch c.Profile.Signer.Mode {
	case "local":
		if localErr != nil {
			return nil, fmt.Errorf("signer.mode is local but cometcli's keyring can't sign: %w", localErr)
		}
		return localB, nil
	case "container":
		return c.containerBuilder()
	case "":
	default:
		return nil, fmt.Errorf("signer.mode %q: want local or container", c.Profile.Signer.Mode)
	}
	switch {
	case localErr == nil && containerOK:
		if c.Chooser == nil {
			return nil, fmt.Errorf("both cometcli's keyring (%s) and the node container's keyring (%s) can sign — set signer.mode to local or container, or run interactively", localB.Address(), container)
		}
		i, err := c.Chooser(c, "Sign this transaction with which key?", []string{
			fmt.Sprintf("cometcli keyring — %s (%s)", c.Profile.Signer.Key, localB.Address()),
			fmt.Sprintf("node container %s — key %s (signed inside the container)", container, ckey),
		})
		if err != nil {
			return nil, err
		}
		if i == 0 {
			return localB, nil
		}
		return c.containerBuilder()
	case localErr == nil:
		return localB, nil
	case containerOK:
		b, err := c.containerBuilder()
		if err != nil {
			return nil, c.cantSign(localErr, err)
		}
		return b, nil
	}
	return nil, c.cantSign(localErr, nil)
}

// cantSign explains, once and completely, why nothing can sign for this
// validator and how to fix it — so nobody goes hunting through keyrings.
func (c *Context) cantSign(localErr, containerErr error) error {
	who := "the signing account"
	if v := c.Profile.Metadata["valoper"]; v != "" {
		if acc, err := keys.AccFromValoper(v); err == nil {
			who = "the operator account " + acc + " (validator " + v + ")"
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "no key available to sign as %s.", who)
	if localErr != nil {
		fmt.Fprintf(&b, "\n  - cometcli's keyring: %v", localErr)
	}
	if containerErr != nil {
		fmt.Fprintf(&b, "\n  - node container %s: %v", c.signerContainer(), containerErr)
	} else if c.signerContainer() == "" {
		b.WriteString("\n  - node container: none configured")
	}
	fmt.Fprintf(&b, "\nfix (operator): import the operator key — `cometcli keys add --name operator --recover` then `cometcli profile add %s --signer operator` —", c.Profile.Name)
	b.WriteString(" or, if the node's own keyring has it under another home/keyring, set signer.container_home / signer.container_keyring.")
	b.WriteString("\nThis is a setup step only the operator can do: stop and tell them; don't look for keys yourself.")
	return fmt.Errorf("%s", b.String())
}

// containerBuilder sets up signing inside the node container: it detects
// the keyring backend and the key's address when not configured, and
// fetches the account's on-chain public key for gas simulation.
func (c *Context) containerBuilder() (*tx.Builder, error) {
	container, key := c.signerContainer(), c.containerKey()
	if container == "" || key == "" {
		return nil, fmt.Errorf("container signing needs signer.container (or a docker service.unit) and signer.container_key")
	}
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	g, err := c.GRPC()
	if err != nil {
		return nil, err
	}
	sc := c.Profile.Signer
	bin := sc.ContainerBinary
	if bin == "" {
		bin = c.Profile.Binary
	}
	if bin == "" {
		bin = "evmd"
	}
	run := func(ctx context.Context, script string, stdin []byte) (string, error) {
		res, err := host.ExecIn(ctx, h, script, stdin, 64<<10)
		if err != nil {
			return res.Output, err
		}
		if res.Code != 0 {
			return res.Output, fmt.Errorf("exit %d", res.Code)
		}
		return res.Output, nil
	}
	home := ""
	if sc.ContainerHome != "" {
		home = " --home " + shq(sc.ContainerHome)
	}
	keysShow := func(backend string, stdin []byte) (string, error) {
		out, err := run(c, fmt.Sprintf("docker exec -i %s %s%s keys show %s -a --keyring-backend %s",
			shq(container), shq(bin), home, shq(key), shq(backend)), stdin)
		return lastLine(out), err
	}
	keyring, addr := sc.ContainerKeyring, ""
	if keyring == "" {
		// the unencrypted test keyring answers without a password
		if a, err := keysShow("test", nil); err == nil && strings.Contains(a, "1") {
			keyring, addr = "test", a
		} else {
			keyring = "file"
		}
	}
	if addr == "" {
		switch {
		case c.Profile.Metadata["account"] != "":
			addr = c.Profile.Metadata["account"]
		case c.Profile.Metadata["valoper"] != "":
			addr, err = keys.AccFromValoper(c.Profile.Metadata["valoper"])
		case keyring == "test":
			addr, err = keysShow("test", nil)
		default:
			var pw string
			if pw, err = c.secret(fmt.Sprintf("Keyring password for %s in %s (to read its address)", key, container)); err == nil {
				addr, err = keysShow(keyring, []byte(pw+"\n"))
			}
		}
		if err != nil {
			return nil, fmt.Errorf("resolving the address of %s in %s: %w", key, container, err)
		}
	}
	pk, _ := g.AccountPubKey(c, addr)
	signer := &tx.ContainerSigner{
		Container: container, Key: key, Keyring: keyring, Home: sc.ContainerHome, Binary: bin, Addr: addr,
		PubKey: pk, Run: run, Secret: c.secret,
	}
	return tx.NewBuilderWith(g, c.Profile, c.Audit, signer), nil
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// ResetSigner forgets the signer choice, so the next TxSigner asks again
// (a new operation on a long-lived context).
func (c *Context) ResetSigner() {
	c.mu.Lock()
	c.signerB = nil
	c.mu.Unlock()
}
