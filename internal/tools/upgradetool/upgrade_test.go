package upgradetool

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func tarball(t *testing.T, name string, body []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func setup(t *testing.T, asset []byte) (*toolkit.Context, string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(asset) }))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	c := &toolkit.Context{Context: context.Background(), Profile: &config.Profile{Name: "t", Home: home, Binary: "evmd"},
		AutoApproveBelow: toolkit.TierOnChain}
	c.SetHost(&host.Local{})
	return c, srv.URL + "/evmd.tar.gz", filepath.Join(home, "cosmovisor", "upgrades", "v0.6.0-to-v0.7.0", "bin", "evmd")
}

func TestPrepareStagesBinaryVerifyingEitherChecksum(t *testing.T) {
	bin := []byte("#!/bin/sh\necho evmd v0.7.2\n")
	for _, layout := range []string{"evmd", "bin/evmd", "evmd_0.7.2_linux_amd64/bin/evmd"} {
		asset := tarball(t, layout, bin)
		for which, cs := range map[string]string{"tarball": sum(asset), "binary": sum(bin)} {
			t.Run(layout+"/"+which, func(t *testing.T) {
				c, url, staged := setup(t, asset)
				res, err := (prepareTool{}).Run(c, toolkit.Args{"name": "v0.6.0-to-v0.7.0", "url": url, "checksum": cs})
				if err != nil {
					t.Fatalf("%v\n%v", err, res)
				}
				b, err := os.ReadFile(staged)
				if err != nil || !bytes.Equal(b, bin) {
					t.Fatalf("staged binary: %v", err)
				}
				if fi, _ := os.Stat(staged); fi.Mode().Perm() != 0o755 {
					t.Fatalf("mode %v", fi.Mode().Perm())
				}
				if res.Data["sha256"] != sum(bin) || res.Data["verified"] != true {
					t.Fatalf("data = %v", res.Data)
				}
			})
		}
	}
}

func TestPrepareRejectsChecksumMismatch(t *testing.T) {
	asset := tarball(t, "evmd", []byte("tampered"))
	c, url, staged := setup(t, asset)
	_, err := (prepareTool{}).Run(c, toolkit.Args{"name": "v0.6.0-to-v0.7.0", "url": url, "checksum": strings.Repeat("ab", 32)})
	if err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if _, err := os.Stat(staged); err == nil {
		t.Fatal("binary staged despite a checksum mismatch")
	}
}

func TestPrepareWithoutChecksumIsFlaggedUnverified(t *testing.T) {
	asset := tarball(t, "evmd", []byte("bin"))
	c, url, _ := setup(t, asset)
	var detail map[string]any
	c.AutoApproveBelow = toolkit.TierLocalChange
	c.Approver = func(_ *toolkit.Context, _ string, _ toolkit.Tier, d map[string]any) (bool, error) {
		detail = d
		return true, nil
	}
	res, err := (prepareTool{}).Run(c, toolkit.Args{"name": "v0.6.0-to-v0.7.0", "url": url})
	if err != nil {
		t.Fatal(err)
	}
	if res.Data["verified"] != false || !strings.Contains(res.Text, "UNVERIFIED") || detail["warning"] == nil {
		t.Fatalf("unverified install not flagged: %v %v", res, detail)
	}
}

func TestPrepareMissingBinaryInArchive(t *testing.T) {
	asset := tarball(t, "README.md", []byte("no binary here"))
	c, url, _ := setup(t, asset)
	if _, err := (prepareTool{}).Run(c, toolkit.Args{"name": "v0.6.0-to-v0.7.0", "url": url}); err == nil || !strings.Contains(err.Error(), "evmd") {
		t.Fatalf("err = %v", err)
	}
}
