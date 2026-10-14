package monitor

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	mdBold   = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdCode   = regexp.MustCompile("`([^`\n]+)`")
	mdItalic = regexp.MustCompile(`(^|[\s(])_([^_\n]+)_([\s).,:;]|$)`)
	mdHead   = regexp.MustCompile(`(?m)^#{1,6} +(.+)$`)
	mdBullet = regexp.MustCompile(`(?m)^(\s*)[-*] +`)
)

// TelegramHTML renders the Markdown cometcli writes (bold, code, italics,
// headings, bullets, "> " quotes — collapsed) as Telegram's HTML parse
// mode, escaping the rest.
func TelegramHTML(md string) string {
	var out []string
	var quote []string
	flush := func() {
		if len(quote) > 0 {
			out = append(out, "<blockquote expandable>"+strings.Join(quote, "\n")+"</blockquote>")
			quote = nil
		}
	}
	for _, line := range strings.Split(md, "\n") {
		if q, ok := strings.CutPrefix(line, "> "); ok || line == ">" {
			quote = append(quote, inlineHTML(q))
			continue
		}
		flush()
		out = append(out, inlineHTML(line))
	}
	flush()
	return strings.Join(out, "\n")
}

func inlineHTML(md string) string {
	var codes []string
	s := mdCode.ReplaceAllStringFunc(md, func(m string) string {
		codes = append(codes, tgEsc(m[1:len(m)-1]))
		return "\x00" + strconv.Itoa(len(codes)-1) + "\x00"
	})
	s = tgEsc(s)
	s = mdHead.ReplaceAllString(s, "<b>$1</b>")
	s = mdBold.ReplaceAllString(s, "<b>$1</b>")
	s = mdItalic.ReplaceAllString(s, "$1<i>$2</i>$3")
	s = mdBullet.ReplaceAllString(s, "$1• ")
	for i, c := range codes {
		s = strings.Replace(s, "\x00"+strconv.Itoa(i)+"\x00", "<code>"+c+"</code>", 1)
	}
	return s
}

// tgEsc escapes what Telegram's HTML mode requires (only &, < and >).
var tgEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
