// Package sshd is a minimal in-process SSH server for tests: public-key
// auth, exec (run with sh on this machine) and direct-tcpip forwarding.
package sshd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Server is a running test sshd.
type Server struct {
	Addr     string
	Host     string
	Port     int
	HostKeys []ssh.Signer // the first is ed25519, the second ECDSA
	Forwards atomic.Int64 // direct-tcpip channels opened

	ln    net.Listener
	mu    sync.Mutex
	conns []*ssh.ServerConn
}

// Start serves on 127.0.0.1 and accepts the given client keys.
func Start(t *testing.T, authorized ...ssh.PublicKey) *Server {
	t.Helper()
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	ed, _ := ssh.NewSignerFromKey(edPriv)
	ecPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ec, _ := ssh.NewSignerFromKey(ecPriv)
	s := &Server{HostKeys: []ssh.Signer{ed, ec}}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			for _, a := range authorized {
				if bytes.Equal(a.Marshal(), k.Marshal()) {
					return nil, nil
				}
			}
			return nil, io.EOF
		},
	}
	for _, k := range s.HostKeys {
		cfg.AddHostKey(k)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln, s.Addr = ln, ln.Addr().String()
	h, p, _ := net.SplitHostPort(s.Addr)
	s.Host, s.Port = h, atoi(p)
	t.Cleanup(func() { ln.Close(); s.DropAll() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, cfg)
		}
	}()
	return s
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// DropAll closes every client connection (a network blip, a reboot).
func (s *Server) DropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

func (s *Server) serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	s.mu.Lock()
	s.conns = append(s.conns, sc)
	s.mu.Unlock()
	go func() {
		for r := range reqs {
			if r.WantReply {
				_ = r.Reply(r.Type == "keepalive@openssh.com", nil)
			}
		}
	}()
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			go session(nc)
		case "direct-tcpip":
			s.Forwards.Add(1)
			go forward(nc)
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "no")
		}
	}
}

func session(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for r := range reqs {
		if r.Type != "exec" {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
			continue
		}
		var p struct{ Cmd string }
		_ = ssh.Unmarshal(r.Payload, &p)
		_ = r.Reply(true, nil)
		cmd := exec.Command("sh", "-c", p.Cmd)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
		code := 0
		if err := cmd.Run(); err != nil {
			code = 1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
		}
		st := make([]byte, 4)
		binary.BigEndian.PutUint32(st, uint32(code))
		_, _ = ch.SendRequest("exit-status", false, st)
		return
	}
}

func forward(nc ssh.NewChannel) {
	var p struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, "bad request")
		return
	}
	dst, err := net.Dial("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		dst.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { _, _ = io.Copy(ch, dst); _ = ch.CloseWrite() }()
	_, _ = io.Copy(dst, ch)
	dst.Close()
	ch.Close()
}

// ClientKey writes a fresh ed25519 key pair (OpenSSH format) under dir and
// returns the private key path and its public key.
func ClientKey(t *testing.T, dir string, passphrase string) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var blockBytes []byte
	if passphrase == "" {
		b, err := ssh.MarshalPrivateKey(priv, "")
		if err != nil {
			t.Fatal(err)
		}
		blockBytes = pemEncode(b.Type, b.Bytes)
	} else {
		b, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
		if err != nil {
			t.Fatal(err)
		}
		blockBytes = pemEncode(b.Type, b.Bytes)
	}
	path := filepath.Join(dir, "id_test")
	if passphrase != "" {
		path += "_locked"
	}
	if err := os.WriteFile(path, blockBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	pk, _ := ssh.NewPublicKey(pub)
	_ = os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(pk), 0o644)
	return path, pk
}

func pemEncode(typ string, b []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b})
}
