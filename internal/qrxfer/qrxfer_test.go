package qrxfer

import (
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosmos/go-bip39"
)

func gentxLike() []byte {
	var b strings.Builder
	b.WriteString(`{"version":1,"chain_id":"primium-3","moniker":"p3-val3","gentx":{"body":{"messages":[`)
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 40; i++ {
		b.WriteString(`{"k":"`)
		for j := 0; j < 40; j++ {
			b.WriteByte("0123456789abcdef"[r.Intn(16)])
		}
		b.WriteString(`"},`)
	}
	b.WriteString(`{}]}}}`)
	return []byte(b.String())
}

func TestRoundTripAnyOrderWithRepeats(t *testing.T) {
	data := gentxLike()
	parts, err := Encode("p3-val3.gentx.json", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 2 {
		t.Fatalf("want several parts for %d bytes, got %d", len(data), len(parts))
	}
	var a Assembler
	for i := len(parts) - 1; i >= 0; i-- { // reversed, with a repeat
		if _, err := a.Add(parts[i]); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if isNew, err := a.Add(parts[i]); err != nil || isNew {
				t.Fatalf("a repeated part should be ignored: %v %v", isNew, err)
			}
		}
	}
	name, got, err := a.File()
	if err != nil || name != "p3-val3.gentx.json" || string(got) != string(data) {
		t.Fatalf("roundtrip: %v %q", err, name)
	}
}

func TestIncompleteForeignAndDamaged(t *testing.T) {
	p1, _ := Encode("a.json", gentxLike())
	p2, _ := Encode("b.json", []byte(`{"other":true}`))
	var a Assembler
	_, _ = a.Add(p1[0])
	if _, _, err := a.File(); err == nil || !strings.Contains(err.Error(), "missing parts") {
		t.Fatalf("incomplete: %v", err)
	}
	if _, err := a.Add(p2[0]); err == nil || !strings.Contains(err.Error(), "another file") {
		t.Fatalf("foreign part accepted: %v", err)
	}
	bad := p1[1][:len(p1[1])-6] + "AAAAAA"
	for _, x := range p1[1:] {
		if x == p1[1] {
			_, _ = a.Add(bad)
			continue
		}
		_, _ = a.Add(x)
	}
	if _, _, err := a.File(); err == nil {
		t.Fatal("a damaged part must not produce a file")
	}
	if _, err := a.Add("hello world"); err == nil {
		t.Fatal("arbitrary text accepted as a part")
	}
}

func TestRefusesKeyMaterial(t *testing.T) {
	e, _ := bip39.NewEntropy(256)
	m, _ := bip39.NewMnemonic(e)
	if _, err := Encode("op.mnemonic", []byte(m)); err == nil || !strings.Contains(err.Error(), "key material") {
		t.Fatalf("a mnemonic must never be QR-encoded: %v", err)
	}
	if _, err := Encode("k.json", []byte(`{"priv_key":{"type":"tendermint/PrivKeyEd25519","value":"q2Ej1vT0Yd8kbm0GhX9wJ0Yv6p7c9W3qkH1sXy8bVwQ0c2Vt9tO3y7mJx3qT8f3h4Lr2Vb6ZpN9w0Qn1aYk5sA=="}}`)); err == nil {
		t.Fatal("a consensus key must never be QR-encoded")
	}
}

// What a phone or webcam reads off the screen: render each part as an
// image and decode it back.
func TestImagesDecode(t *testing.T) {
	data := gentxLike()
	parts, _ := Encode("g.json", data)
	var a Assembler
	for i, p := range parts {
		f := filepath.Join(t.TempDir(), "part.png")
		if err := PNG(p, f); err != nil {
			t.Fatal(err)
		}
		txt, err := DecodeImage(f)
		if err != nil {
			t.Fatalf("part %d: %v", i+1, err)
		}
		if _, err := a.Add(txt); err != nil {
			t.Fatal(err)
		}
	}
	if _, got, err := a.File(); err != nil || string(got) != string(data) {
		t.Fatalf("decoded images: %v", err)
	}
	s, err := Terminal(parts[0])
	if err != nil || len(strings.Split(s, "\n")) < 20 {
		t.Fatalf("terminal rendering: %v", err)
	}
	if w := len([]rune(strings.Split(s, "\n")[0])); w > 80 {
		t.Errorf("a code is %d columns wide; it must fit an 80-column terminal", w)
	}
}
