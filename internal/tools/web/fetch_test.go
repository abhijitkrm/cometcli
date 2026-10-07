package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func TestHTMLText(t *testing.T) {
	in := `<html><head><title>x</title><style>p{}</style></head><body><nav>menu</nav>
<h2>Upgrade v0.7.2</h2><p>Halt at height <b>1200</b> &amp; swap.</p><ul><li>stop node</li><li>replace binary</li></ul>
<script>alert(1)</script></body></html>`
	out := HTMLText(in)
	for _, want := range []string{"## Upgrade v0.7.2", "Halt at height 1200 & swap.", "- stop node", "- replace binary"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	for _, bad := range []string{"alert", "menu", "p{}", "<"} {
		if strings.Contains(out, bad) {
			t.Errorf("kept %q in %q", bad, out)
		}
	}
}

func TestFetchLocalNoPromptExternalAsks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"height":"42"}`))
	}))
	defer srv.Close()
	asked := 0
	c := &toolkit.Context{Context: context.Background(), AutoApproveBelow: toolkit.TierLocalChange,
		Approver: func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { asked++; return false, nil }}
	res, err := (Fetch{}).Run(c, toolkit.Args{"url": srv.URL})
	if err != nil || !strings.Contains(res.Text, `"height":"42"`) || asked != 0 {
		t.Fatalf("local fetch: %v asked=%d %v", res, asked, err)
	}
	if _, err := (Fetch{}).Run(c, toolkit.Args{"url": "https://example.com/"}); err == nil || asked != 1 {
		t.Fatalf("external fetch must ask: err=%v asked=%d", err, asked)
	}
	if _, err := (Fetch{}).Run(c, toolkit.Args{"url": "file:///etc/passwd"}); err == nil {
		t.Fatal("file URL accepted")
	}
}
