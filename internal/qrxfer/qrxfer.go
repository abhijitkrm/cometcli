// Package qrxfer moves a small public file off an air-gapped machine as a
// series of QR codes: shown on its screen, scanned by a phone or camera on
// a connected machine, and reassembled there. Nothing is ever plugged into
// the offline machine.
//
// Each code is one line of text:
//
//	CQR1|<sha256 of the file>|<part>|<parts>|<file name>|<base64url of a gzip chunk>
//
// so parts can be scanned in any order, repeats are harmless, a part of
// another file is refused, and the file is written only when it hashes to
// the sum every part carries.
package qrxfer

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/jpeg" // photos of the screen
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/makiuchi-d/gozxing"
	gzqr "github.com/makiuchi-d/gozxing/qrcode"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/abhijitkrm/cometcli/internal/egress"
)

const (
	magic = "CQR1"
	// ChunkSize is the compressed bytes per code: small enough that a code
	// fits an 80-column terminal and scans from a phone.
	ChunkSize = 160
	// MaxFile bounds what QR transfer is for: join and gentx files, a few KB.
	MaxFile = 64 << 10
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// Encode splits a file into QR payloads. It refuses anything that carries
// key material: QR transfer is for public files only.
func Encode(name string, data []byte) ([]string, error) {
	name = filepath.Base(name)
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("file name %q: letters, digits, . _ - only", name)
	}
	if len(data) > MaxFile {
		return nil, fmt.Errorf("%s is %d bytes — QR transfer is for small files (join, gentx: a few KB)", name, len(data))
	}
	if f := egress.Scan(string(data)); len(f) > 0 {
		return nil, fmt.Errorf("%s contains key material (%s) — it must never leave the machine, by QR or otherwise", name, egress.Kinds(f))
	}
	sum := sha256.Sum256(data)
	var z bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&z, gzip.BestCompression)
	_, _ = zw.Write(data)
	_ = zw.Close()
	packed := z.Bytes()
	n := (len(packed) + ChunkSize - 1) / ChunkSize
	if n == 0 {
		n = 1
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lo, hi := i*ChunkSize, min((i+1)*ChunkSize, len(packed))
		parts = append(parts, strings.Join([]string{magic, hex.EncodeToString(sum[:]), strconv.Itoa(i + 1), strconv.Itoa(n), name,
			base64.RawURLEncoding.EncodeToString(packed[lo:hi])}, "|"))
	}
	return parts, nil
}

// Terminal renders a payload as a QR code with half-block characters.
func Terminal(payload string) (string, error) {
	q, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return "", err
	}
	return q.ToSmallString(false), nil
}

// PNG writes a payload as a QR code image.
func PNG(payload, path string) error {
	return qrcode.WriteFile(payload, qrcode.Medium, 640, path)
}

// Assembler collects scanned parts until the file is complete.
type Assembler struct {
	sum   string
	name  string
	n     int
	parts map[int][]byte
}

// Add takes one scanned line. It reports whether the part was new.
func (a *Assembler) Add(line string) (bool, error) {
	f := strings.Split(strings.TrimSpace(line), "|")
	if len(f) != 6 || f[0] != magic {
		return false, fmt.Errorf("not a cometcli QR part")
	}
	i, err1 := strconv.Atoi(f[2])
	n, err2 := strconv.Atoi(f[3])
	if err1 != nil || err2 != nil || n < 1 || n > 1000 || i < 1 || i > n || len(f[1]) != 64 || !nameRe.MatchString(f[4]) {
		return false, fmt.Errorf("damaged QR part")
	}
	if a.sum == "" {
		a.sum, a.name, a.n, a.parts = f[1], f[4], n, map[int][]byte{}
	}
	if f[1] != a.sum || n != a.n {
		return false, fmt.Errorf("this part belongs to another file (%s), not %s — scan one file at a time", f[4], a.name)
	}
	chunk, err := base64.RawURLEncoding.DecodeString(f[5])
	if err != nil {
		return false, fmt.Errorf("damaged QR part %d", i)
	}
	if old, ok := a.parts[i]; ok {
		if !bytes.Equal(old, chunk) {
			return false, fmt.Errorf("part %d scanned twice with different content", i)
		}
		return false, nil
	}
	a.parts[i] = chunk
	return true, nil
}

// Progress is (parts held, parts needed, file name).
func (a *Assembler) Progress() (int, int, string) { return len(a.parts), a.n, a.name }

// Missing lists the parts not scanned yet.
func (a *Assembler) Missing() []int {
	var m []int
	for i := 1; i <= a.n; i++ {
		if _, ok := a.parts[i]; !ok {
			m = append(m, i)
		}
	}
	return m
}

// File reassembles and verifies the file.
func (a *Assembler) File() (string, []byte, error) {
	if a.n == 0 || len(a.parts) < a.n {
		return "", nil, fmt.Errorf("missing parts %v", a.Missing())
	}
	var packed []byte
	for i := 1; i <= a.n; i++ {
		packed = append(packed, a.parts[i]...)
	}
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return "", nil, fmt.Errorf("the parts don't form a file: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(zr, MaxFile+1))
	if err != nil || len(data) > MaxFile {
		return "", nil, fmt.Errorf("the parts don't form a file")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != a.sum {
		return "", nil, fmt.Errorf("the reassembled file doesn't match its checksum — rescan")
	}
	return a.name, data, nil
}

// DecodeImage reads every cometcli QR code in an image file (a photo or
// screenshot of the offline machine's screen).
func DecodeImage(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", err
	}
	// a photo needs the detector; a clean screenshot sometimes decodes
	// only as a pure code
	for _, pure := range []bool{false, true} {
		hints := map[gozxing.DecodeHintType]interface{}{gozxing.DecodeHintType_TRY_HARDER: true}
		if pure {
			hints[gozxing.DecodeHintType_PURE_BARCODE] = true
		}
		if res, err := gzqr.NewQRCodeReader().Decode(bmp, hints); err == nil {
			return res.GetText(), nil
		}
	}
	return "", fmt.Errorf("%s: no QR code found — retake it straight on, the whole code in frame", path)
}
