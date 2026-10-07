// Package web implements the agent's web_fetch tool.
package web

import (
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Register adds web_fetch.
func Register(r *toolkit.Registry) { r.Register(Fetch{}) }

const maxBody = 5 << 20

// Fetch GETs a URL from the machine cometcli runs on.
type Fetch struct{}

func (Fetch) Name() string { return "web_fetch" }
func (Fetch) Desc() string {
	return "Fetch a URL (http/https GET) from the operator's machine and return it as text — release notes, docs, upgrade instructions, a node's REST/RPC endpoint. The operator approves each new domain; localhost and private addresses are fetched directly. Redirects to another host are reported, not followed."
}
func (Fetch) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"url": toolkit.Str("absolute http(s) URL"),
	}, "url")
}
func (Fetch) Tier() toolkit.Tier { return toolkit.TierDiagnose }
func (Fetch) AgentOnly() bool    { return true }
func (Fetch) OutputLimit() int   { return 30_000 }

func (Fetch) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	raw := strings.TrimSpace(a.String("url", ""))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("want an absolute http(s) URL, got %q", raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("URLs with embedded credentials are refused")
	}
	if err := c.Check(toolkit.Gate{
		Request:      toolkit.Request{Tool: "web_fetch", Specs: []string{u.String()}, Kind: "url"},
		Tier:         toolkit.TierDiagnose,
		Prompt:       "fetch " + u.String(),
		Detail:       map[string]any{"domain": u.Hostname()},
		AskByDefault: !privateHost(u.Hostname()),
	}); err != nil {
		return nil, err
	}
	hc := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Hostname() != u.Hostname() {
				return http.ErrUseLastResponse
			}
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(c, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cometcli (+https://github.com/abhijitkrm/cometcli)")
	req.Header.Set("Accept", "text/html,text/plain,application/json,text/markdown;q=0.9,*/*;q=0.5")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 3 {
		return &toolkit.Result{Text: fmt.Sprintf("HTTP %d redirect to %s — a different host; fetch that URL if it should be followed", resp.StatusCode, resp.Header.Get("Location"))}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	text := string(body)
	if strings.Contains(ct, "html") || (ct == "" && strings.Contains(strings.ToLower(text[:min(len(text), 512)]), "<html")) {
		text = HTMLText(text)
	} else if !strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "json") && !strings.Contains(ct, "xml") && !strings.Contains(ct, "yaml") && ct != "" {
		return &toolkit.Result{Text: fmt.Sprintf("HTTP %d, %s, %d bytes — not text; download it with bash (curl -o) if needed", resp.StatusCode, ct, len(body))}, nil
	}
	head := fmt.Sprintf("%s — HTTP %d %s\n\n", u.String(), resp.StatusCode, ct)
	return &toolkit.Result{Text: head + strings.TrimSpace(text)}, nil
}

// privateHost reports loopback, link-local and RFC1918 targets: a node's
// own RPC endpoints, fetched without a domain prompt.
func privateHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

var (
	dropRe  = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head|nav|footer)\b.*?</(script|style|noscript|svg|head|nav|footer)>|<!--.*?-->`)
	breakRe = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/h[1-6]|/tr|/pre|/section|/article|/table|hr)\b[^>]*>`)
	itemRe  = regexp.MustCompile(`(?i)<\s*li\b[^>]*>`)
	headRe  = regexp.MustCompile(`(?i)<\s*h([1-6])\b[^>]*>`)
	tagRe   = regexp.MustCompile(`(?s)<[^>]+>`)
	blankRe = regexp.MustCompile(`\n[ \t]*\n([ \t]*\n)+`)
	spaceRe = regexp.MustCompile(`[ \t]+`)
)

// HTMLText reduces an HTML page to readable text: scripts, styles and
// chrome dropped, block ends turned into line breaks, list items and
// headings marked, entities decoded.
func HTMLText(s string) string {
	s = dropRe.ReplaceAllString(s, "")
	s = headRe.ReplaceAllStringFunc(s, func(m string) string {
		n := headRe.FindStringSubmatch(m)[1]
		return "\n" + strings.Repeat("#", int(n[0]-'0')) + " "
	})
	s = itemRe.ReplaceAllString(s, "\n- ")
	s = breakRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spaceRe.ReplaceAllString(l, " "))
	}
	return blankRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
}
