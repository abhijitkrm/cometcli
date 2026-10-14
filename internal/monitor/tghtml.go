package monitor

import (
	"html"
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
// headings, bullets) as Telegram's HTML parse mode, escaping the rest.
func TelegramHTML(md string) string {
	var codes []string
	s := mdCode.ReplaceAllStringFunc(md, func(m string) string {
		codes = append(codes, html.EscapeString(m[1:len(m)-1]))
		return "\x00" + strconv.Itoa(len(codes)-1) + "\x00"
	})
	s = html.EscapeString(s)
	s = mdHead.ReplaceAllString(s, "<b>$1</b>")
	s = mdBold.ReplaceAllString(s, "<b>$1</b>")
	s = mdItalic.ReplaceAllString(s, "$1<i>$2</i>$3")
	s = mdBullet.ReplaceAllString(s, "$1• ")
	for i, c := range codes {
		s = strings.Replace(s, "\x00"+strconv.Itoa(i)+"\x00", "<code>"+c+"</code>", 1)
	}
	return s
}
