// Package shell implements the general-purpose `bash` tool: shell
// commands on the profile's host (local or SSH), with a per-call risk
// classification that drives the permission gate.
package shell

import (
	"path"
	"regexp"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Verdict is the classification of a whole command line.
type Verdict struct {
	Tier      toolkit.Tier
	Reason    string   // why the tier is what it is (shown when asking)
	Forbidden string   // non-empty: refuse in every mode
	Segments  []string // normalized simple commands, for rule matching
	Notes     []string // advisories (e.g. the script prompts for input)
}

// Opts tunes classification for the session's node.
type Opts struct {
	// Binary is the profile's chain binary (e.g. "evmd"), recognized
	// alongside the generic "<name>d" convention.
	Binary string
	// ReadScript returns a script file's content (relative paths resolve
	// against the working directory); ok=false when unreadable.
	ReadScript func(path string) (content []byte, ok bool)
}

type acc struct {
	opts    Opts
	v       Verdict
	depth   int
	cmdText string // the whole command line, heredocs included
}

var (
	copiers      = map[string]bool{"cp": true, "install": true, "ln": true, "rsync": true, "scp": true}
	interpreters = map[string]bool{"python": true, "python3": true, "python2": true, "perl": true, "node": true, "ruby": true, "php": true, "jq": false}
	// a node's data dir (or what's in it), by name or by the stores it holds
	nodeDataRe = regexp.MustCompile(`(^|/)data/?(\*)?$|(^|/)data/(\*|application\.db|blockstore\.db|state\.db|tx_index\.db|evidence\.db|cs\.wal|snapshots)(/|$)|(application|blockstore|state|evidence)\.db/?$|(^|/)cs\.wal(/|$)`)
)

func (a *acc) raise(t toolkit.Tier, reason string) {
	if t > a.v.Tier || a.v.Reason == "" && t == a.v.Tier && t >= toolkit.TierLocalChange {
		a.v.Tier, a.v.Reason = t, reason
	}
}

func (a *acc) forbid(reason string) {
	if a.v.Forbidden == "" {
		a.v.Forbidden = reason
	}
}

func (a *acc) note(n string) {
	for _, x := range a.v.Notes {
		if x == n {
			return
		}
	}
	a.v.Notes = append(a.v.Notes, n)
}

// Classify decides the effective tier of a command line: the highest tier
// of any simple command in it, with redirects to files, substitutions,
// wrappers (sudo, env, xargs, docker exec, bash -c) and invoked scripts
// all taken into account. Unknown commands are local-change (ask).
func Classify(cmd string, o Opts) Verdict {
	a := &acc{opts: o, v: Verdict{Tier: toolkit.TierDiagnose}, cmdText: cmd}
	a.line(cmd)
	return a.v
}

func (a *acc) line(cmd string) {
	if a.depth > 4 {
		a.raise(toolkit.TierLocalChange, "deeply nested command")
		return
	}
	a.depth++
	defer func() { a.depth-- }()
	if strings.Contains(cmd, ":(){") || strings.Contains(cmd, ":() {") {
		a.forbid("fork bomb")
	}
	segs, subs, writes := lex(cmd)
	for _, w := range writes {
		if hit, why := protectedPath(w); hit {
			a.forbid("writes to " + why)
		}
		if strings.Contains(w, "priv_validator_state.json") {
			a.forbid("overwriting priv_validator_state.json risks double-signing")
		}
		a.raise(toolkit.TierLocalChange, "writes to "+w)
	}
	for _, s := range subs {
		a.line(s)
	}
	for _, words := range segs {
		if len(words) == 0 {
			continue
		}
		a.v.Segments = append(a.v.Segments, strings.Join(words, " "))
		a.simple(words)
	}
}

// --- lexer ---------------------------------------------------------------

// lex splits a command line into simple commands (as word lists, quotes
// removed), the bodies of $(…), `…` and <(…) substitutions, and the
// targets of output redirections other than /dev/null and fd dups.
func lex(s string) (segs [][]string, subs []string, writes []string) {
	var words []string
	var cur strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	endSeg := func() {
		endWord()
		if len(words) > 0 {
			segs = append(segs, words)
		}
		words = nil
	}
	pendingRedirect := false
	finishWord := func() {
		if pendingRedirect && inWord {
			t := cur.String()
			if t != "/dev/null" && t != "/dev/stdout" && t != "/dev/stderr" && !strings.HasPrefix(t, "&") {
				writes = append(writes, t)
			}
			cur.Reset()
			inWord = false
			pendingRedirect = false
			return
		}
		endWord()
	}
	// readSub consumes a balanced (...) starting after the opening paren.
	readSub := func(i int) (string, int) {
		depth, j := 1, i
		var q byte
		for ; j < len(s); j++ {
			c := s[j]
			switch {
			case q != 0:
				if c == q {
					q = 0
				} else if c == '\\' && q == '"' {
					j++
				}
			case c == '\'' || c == '"':
				q = c
			case c == '\\':
				j++
			case c == '(':
				depth++
			case c == ')':
				depth--
				if depth == 0 {
					return s[i:j], j
				}
			}
		}
		return s[i:], len(s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			if i+1 < len(s) {
				if s[i+1] != '\n' {
					cur.WriteByte(s[i+1])
					inWord = true
				}
				i++
			}
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			cur.WriteString(s[i+1 : i+1+j])
			inWord = true
			i += j + 1
		case '"':
			inWord = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				switch {
				case s[i] == '\\' && i+1 < len(s):
					i++
					cur.WriteByte(s[i])
				case s[i] == '$' && i+1 < len(s) && s[i+1] == '(':
					body, end := readSub(i + 2)
					subs = append(subs, body)
					cur.WriteString("$(" + body + ")")
					i = end
				case s[i] == '`':
					j := strings.IndexByte(s[i+1:], '`')
					if j < 0 {
						j = len(s) - i - 1
					}
					subs = append(subs, s[i+1:i+1+j])
					i += j + 1
				default:
					cur.WriteByte(s[i])
				}
			}
		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				body, end := readSub(i + 2)
				subs = append(subs, body)
				cur.WriteString("$(" + body + ")")
				inWord = true
				i = end
			} else {
				cur.WriteByte(c)
				inWord = true
			}
		case '`':
			j := strings.IndexByte(s[i+1:], '`')
			if j < 0 {
				j = len(s) - i - 1
			}
			subs = append(subs, s[i+1:i+1+j])
			i += j + 1
		case '<':
			if i+1 < len(s) && s[i+1] == '(' { // process substitution
				body, end := readSub(i + 2)
				subs = append(subs, body)
				i = end
				continue
			}
			finishWord()
			if i+1 < len(s) && s[i+1] == '<' { // heredoc / herestring
				i++
				if i+1 < len(s) && s[i+1] == '<' {
					i++ // <<< herestring: the word that follows is data
					continue
				}
				// <<[-]DELIM: the body up to a line holding DELIM is data,
				// not commands — skip it, keeping the rest of this line
				j := i + 1
				if j < len(s) && s[j] == '-' {
					j++
				}
				for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
					j++
				}
				k := j
				for k < len(s) && !strings.ContainsRune(" \t\n;&|<>()", rune(s[k])) {
					k++
				}
				delim := strings.Trim(s[j:k], `'"`)
				if delim == "" {
					continue
				}
				nl := strings.IndexByte(s[k:], '\n')
				if nl < 0 {
					i = k - 1
					continue
				}
				bodyStart := k + nl + 1
				end := len(s)
				for pos := bodyStart; pos < len(s); {
					e := strings.IndexByte(s[pos:], '\n')
					line := s[pos:]
					if e >= 0 {
						line = s[pos : pos+e]
					}
					if strings.TrimLeft(line, "\t") == delim {
						end = pos + len(line)
						break
					}
					if e < 0 {
						break
					}
					pos += e + 1
				}
				// lex the rest of the heredoc's own line, then resume after
				// the delimiter line
				restSegs, restSubs, restWrites := lex(s[k : k+nl])
				if len(restSegs) > 0 {
					words = append(words, restSegs[0]...)
					segs = append(segs, restSegs[1:]...)
				}
				subs = append(subs, restSubs...)
				writes = append(writes, restWrites...)
				i = end - 1
				continue
			}
		case '>':
			// "2>&1", ">&2" are fd dups; "N>" redirects fd N
			if inWord && isDigits(cur.String()) {
				cur.Reset()
				inWord = false
			} else {
				finishWord()
			}
			if i+1 < len(s) && (s[i+1] == '>' || s[i+1] == '|') {
				i++
			}
			if i+1 < len(s) && s[i+1] == '&' {
				i++
				for i+1 < len(s) && (isDigit(s[i+1]) || s[i+1] == '-') {
					i++
				}
				continue
			}
			pendingRedirect = true
		case ';', '\n':
			finishWord()
			endSeg()
		case '&':
			if i+1 < len(s) && s[i+1] == '>' { // &> file
				finishWord()
				i++
				if i+1 < len(s) && s[i+1] == '>' {
					i++
				}
				pendingRedirect = true
				continue
			}
			finishWord()
			endSeg()
			if i+1 < len(s) && s[i+1] == '&' {
				i++
			}
		case '|':
			finishWord()
			endSeg()
			if i+1 < len(s) && (s[i+1] == '|' || s[i+1] == '&') {
				i++
			}
		case '(', ')', '{', '}':
			if inWord {
				cur.WriteByte(c)
			} else if c == '(' || c == ')' {
				endSeg()
			} else {
				// lone { } group delimiters
				finishWord()
			}
		case ' ', '\t', '\r':
			finishWord()
		case '#':
			if !inWord { // comment to end of line
				j := strings.IndexByte(s[i:], '\n')
				if j < 0 {
					i = len(s)
				} else {
					i += j - 1
				}
				continue
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	finishWord()
	endSeg()
	return segs, subs, writes
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// --- sensitive material --------------------------------------------------

var protectedRe = regexp.MustCompile(`priv_validator_key\.json|node_key\.json|(^|/)\.?mnemonics?(\.txt)?(/|$)|mnemonic|keyring-(file|test|os)|/\.ssh/id_|\.cometcli/keys|\.gnupg/|(^|/)keystore(/|$)|(^|/)utc--[0-9]|key_seed\.json`)

func protectedPath(word string) (bool, string) {
	if m := protectedRe.FindString(strings.ToLower(word)); m != "" {
		return true, "protected key material (" + strings.Trim(m, "/") + ")"
	}
	return false, ""
}

// permOnly commands may name key files without reading them: permission
// audits and fixes are legitimate.
var permOnly = map[string]bool{"ls": true, "stat": true, "chmod": true, "chown": true, "test": true, "[": true, "du": true, "file": true}

// --- simple commands -----------------------------------------------------

var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// daemonNames end in "d" but aren't chain binaries.
var daemonNames = map[string]bool{"sshd": true, "systemd": true, "dockerd": true, "containerd": true, "chronyd": true, "named": true, "httpd": true, "crond": true, "rsyslogd": true, "ntpd": true, "launchd": true, "find": true, "fd": true, "sed": true, "head": true, "pwd": true, "cd": true, "add": true, "rmd": true,
	"dd": true, "chmod": true, "read": true, "kind": true, "bind": true, "cmd": true, "od": true, "xxd": true, "fold": true,
	"expand": true, "unexpand": true, "etcd": true, "uuidd": true, "md": true}

func (a *acc) isChainBinary(word string) bool {
	b := path.Base(word)
	if a.opts.Binary != "" && b == path.Base(a.opts.Binary) {
		return true
	}
	return len(b) > 2 && strings.HasSuffix(b, "d") && !daemonNames[b] && chainNameRe.MatchString(b)
}

var chainNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*d$`)

var broadcastRe = regexp.MustCompile(`eth_sendRawTransaction|eth_sendTransaction|broadcast_tx_(sync|async|commit)|/cosmos/tx/v1beta1/txs|BroadcastTx`)

// onChain spots a transaction broadcast anywhere in a simple command,
// including when wrapped (docker exec <c> evmd tx …).
func (a *acc) onChain(words []string) (bool, string) {
	for i, w := range words {
		if a.isChainBinary(w) {
			for j := i + 1; j+1 < len(words); j++ {
				if words[j] == "tx" && !strings.HasPrefix(words[j+1], "-") {
					for _, f := range words[j:] {
						if f == "--generate-only" || f == "--dry-run" {
							return false, ""
						}
					}
					return true, "broadcasts a transaction (" + path.Base(w) + " tx " + words[j+1] + ")"
				}
			}
		}
		if path.Base(w) == "cast" && i+1 < len(words) {
			switch words[i+1] {
			case "send", "publish":
				return true, "broadcasts a transaction (cast " + words[i+1] + ")"
			}
		}
	}
	if m := broadcastRe.FindString(strings.Join(words, " ")); m != "" {
		return true, "broadcasts a transaction (" + m + ")"
	}
	return false, ""
}

func (a *acc) simple(words []string) {
	// leading VAR=value assignments
	for len(words) > 0 && assignRe.MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return
	}
	if ok, why := a.onChain(words); ok {
		a.raise(toolkit.TierOnChain, why)
	}
	name := path.Base(words[0])
	args := words[1:]

	for _, w := range words {
		if hit, why := protectedPath(w); hit && !permOnly[name] {
			a.forbid("touches " + why)
		}
	}
	if (name == "rm" || name == "mv" || name == "truncate" || name == "shred") && containsWord(args, "priv_validator_state.json") {
		a.forbid("removing priv_validator_state.json risks double-signing")
	}
	// copying over the double-sign guard: cp/install/ln/rsync to it, dd of=
	if copiers[name] {
		if dst := lastNonFlag(args); strings.Contains(dst, "priv_validator_state.json") || (strings.HasSuffix(dst, "/data") || strings.HasSuffix(dst, "/data/")) && containsWord(args, "priv_validator_state") {
			a.forbid("overwriting priv_validator_state.json risks double-signing")
		}
	}
	if name == "dd" && containsWord(args, "of=") && containsWord(args, "priv_validator_state.json") {
		a.forbid("overwriting priv_validator_state.json risks double-signing")
	}
	// deleting a node's data directory is a state reset, and takes the
	// double-sign guard (priv_validator_state.json) with it
	if name == "rm" || name == "shred" || (name == "find" && containsWord(args, "-delete")) {
		for _, x := range nonFlags(args) {
			if nodeDataRe.MatchString(x) {
				a.forbid("deleting " + x + " wipes the node's chain data and priv_validator_state.json (a state reset, double-sign risk) — the operator must do it")
				break
			}
		}
	}
	if interpreters[name] && a.cmdText != "" && strings.Contains(a.cmdText, "priv_validator_state") {
		a.forbid("a script that touches priv_validator_state.json risks double-signing — the operator handles that file")
	}

	switch name {
	case "sudo", "doas":
		a.raise(toolkit.TierLocalChange, "runs as root ("+name+")")
		a.simple(skipFlags(args, "-u", "-g", "-C", "-h", "-p", "-U"))
		return
	case "env":
		rest := skipFlags(args, "-u", "-C", "-S")
		for len(rest) > 0 && assignRe.MatchString(rest[0]) {
			rest = rest[1:]
		}
		a.simple(rest)
		return
	case "time", "nice", "nohup", "command", "exec", "builtin", "stdbuf", "ionice", "chrt", "unbuffer":
		if name == "command" && len(args) > 0 && (args[0] == "-v" || args[0] == "-V") {
			return
		}
		a.simple(skipFlags(args, "-n", "-c", "-p", "-o", "-e", "-i"))
		return
	case "timeout":
		rest := skipFlags(args, "-s", "-k", "--signal", "--kill-after")
		if len(rest) > 0 {
			rest = rest[1:] // duration
		}
		a.simple(rest)
		return
	case "watch":
		a.simple(skipFlags(args, "-n", "-d", "--interval"))
		return
	case "xargs":
		rest := skipFlags(args, "-n", "-I", "-P", "-L", "-d", "-E", "-s", "-a")
		if len(rest) == 0 {
			return // xargs alone is echo
		}
		a.simple(rest)
		return
	case "bash", "sh", "zsh", "dash", "ksh":
		for i, x := range args {
			if x == "-c" && i+1 < len(args) {
				a.line(args[i+1])
				return
			}
		}
		if s := firstNonFlag(args); s != "" {
			a.script(s)
			return
		}
		a.raise(toolkit.TierLocalChange, "starts a shell")
		return
	case "source", ".":
		if len(args) > 0 {
			a.script(args[0])
		}
		return
	}

	if fn, ok := checkers[name]; ok {
		if ok, why := fn(args); !ok {
			if why == "" {
				why = name + " " + strings.Join(firstN(args, 2), " ")
			}
			a.raise(toolkit.TierLocalChange, strings.TrimSpace(why))
		}
		return
	}
	if fn, ok := wrappers[name]; ok {
		fn(a, args)
		return
	}
	if readOnly[name] {
		return
	}
	if a.isChainBinary(words[0]) {
		a.chainCmd(name, args)
		return
	}
	if strings.Contains(words[0], "/") || strings.HasSuffix(name, ".sh") {
		a.script(words[0])
		return
	}
	a.raise(toolkit.TierLocalChange, "runs "+name)
}

// script classifies running a script file: local-change at least, and
// on-chain when its body broadcasts transactions.
func (a *acc) script(p string) {
	a.raise(toolkit.TierLocalChange, "runs script "+p)
	if a.opts.ReadScript == nil {
		return
	}
	raw, ok := a.opts.ReadScript(p)
	if !ok {
		return
	}
	body := stripComments(raw)
	if m := scriptTxRe.Find(body); m != nil {
		a.raise(toolkit.TierOnChain, "script "+p+" broadcasts transactions ("+strings.TrimSpace(string(m))+"…)")
	}
	if m := scriptKeyRe.Find(body); m != nil {
		a.forbid("script " + p + " handles key material (" + strings.TrimSpace(string(m)) + ") and would print it into the conversation — the operator runs it in their own terminal")
	}
	if scriptPromptRe.Match(body) {
		a.note("script " + p + " prompts for input (read -p) — stdin is closed here, so prompts get EOF; check for a --yes/non-interactive flag, or ask the operator to run it in their terminal")
	}
	if scriptSudoRe.Match(body) {
		a.note("script " + p + " uses sudo — without cached credentials it fails asking for a password; the operator may need to run it")
	}
	if strings.Contains(string(body), "unsafe-reset-all") {
		a.note("script " + p + " contains unsafe-reset-all")
	}
}

var (
	// a chain binary (or $BINARY-style variable) … tx <module>: the module
	// list keeps prose ("enabled, tx indexer") from matching
	scriptTxRe     = regexp.MustCompile(`(?m)(\b[a-z][a-z0-9_-]*d\b|\$\{?[A-Z_]*(BIN|BINARY|DAEMON|CMD)[A-Z_]*\}?)[^\n]{0,200}?\btx\s+(bank|staking|gov|slashing|distribution|upgrade|ibc|ibc-transfer|transfer|evm|vm|erc20|feemarket|authz|feegrant|vesting|crisis|wasm|precisebank|consensus|circuit|group|nft|mint|ratelimit|ics20|interchain-accounts)\b|\bcast\s+send\b|\beth_sendRawTransaction\b`)
	scriptSudoRe   = regexp.MustCompile(`(?m)^[^#\n]*\bsudo\s`)
	scriptKeyRe    = regexp.MustCompile(`\bkeys\s+(add|export|mnemonic|import|unsafe-export-eth-key)\b|\bcast\s+wallet\s+(new|new-mnemonic|private-key|decrypt-keystore)`)
	scriptPromptRe = regexp.MustCompile(`(?m)\bread\s+(-[a-zA-Z]+\s+)*-[a-zA-Z]*p`)
)

func (a *acc) chainCmd(name string, args []string) {
	sub := firstNonFlag(args)
	switch sub {
	case "q", "query", "status", "version", "debug", "validate-genesis", "validate", "export", "help", "":
		return
	case "keys":
		switch firstNonFlag(args[indexOf(args, "keys")+1:]) {
		case "list", "show", "parse", "":
			return
		}
		a.forbid("key management (" + name + " keys …) handles key material — the operator runs these")
		return
	case "comet", "tendermint", "cometbft":
		rest := args[indexOf(args, sub)+1:]
		s2 := firstNonFlag(rest)
		if strings.HasPrefix(s2, "unsafe-reset") || s2 == "reset-state" {
			a.forbid(name + " " + sub + " " + s2 + " wipes chain state — the operator must run it")
			return
		}
		if strings.HasPrefix(s2, "show-") || s2 == "version" {
			return
		}
	case "unsafe-reset-all":
		a.forbid(name + " unsafe-reset-all wipes chain state — the operator must run it")
		return
	case "start":
		if hasFlag(args, "--help", "-h") {
			return
		}
		a.forbid("starting " + name + " by hand runs a second signer next to the node's service (double-sign risk) — start it with node.service")
		return
	case "init":
		if hasFlag(args, "--help", "-h") {
			return
		}
		a.forbid(name + " init writes new node and validator keys — the operator does it")
		return
	case "rollback":
		a.forbid(name + " rollback rewrites chain state — the operator must run it")
		return
	case "config":
		if len(args) > 1 && (args[1] == "get" || args[1] == "view") {
			return
		}
	case "tx":
		return // onChain already raised it (or it's --generate-only)
	}
	a.raise(toolkit.TierLocalChange, "runs "+name+" "+sub)
}

// --- tables --------------------------------------------------------------

var readOnly = setOf(`ls cat head tail less more wc uniq cut tr nl column diff cmp comm grep egrep fgrep rg ag jq
	stat file du df free uptime uname whoami id groups cal which whereis type echo printf pwd true false test [
	sleep basename dirname realpath readlink printenv ps pgrep vmstat iostat mpstat sar lsof ss netstat
	traceroute tracepath dig nslookup host nproc lscpu lsblk lsmem lspci lsusb findmnt blkid last w who getent
	sha256sum sha1sum sha512sum shasum md5sum b2sum cksum xxd hexdump od strings tree fd zcat zgrep bzcat xzcat
	cd pushd popd export unset set alias seq yes expr bc factor numfmt locale tty arch sw_vers system_profiler
	vm_stat top htop btop atop iotop nload ifstat grpcurl`)

var checkers = map[string]func([]string) (bool, string){
	"date":     func(a []string) (bool, string) { return !hasFlag(a, "-s", "--set"), "sets the clock" },
	"hostname": func(a []string) (bool, string) { return firstNonFlag(a) == "", "sets the hostname" },
	"sort":     func(a []string) (bool, string) { return !hasFlagPrefix(a, "-o", "--output"), "writes a file" },
	"tee":      func(a []string) (bool, string) { return allAre(nonFlags(a), "/dev/null"), "writes files" },
	"sed":      func(a []string) (bool, string) { return !sedInPlace(a), "edits files in place" },
	"awk":      awkOK, "gawk": awkOK, "mawk": awkOK,
	"yq":   func(a []string) (bool, string) { return !hasFlagPrefix(a, "-i", "--inplace"), "edits files in place" },
	"find": findOK,
	"curl": curlOK,
	"wget": func(a []string) (bool, string) {
		return hasFlag(a, "-O-", "-qO-", "--spider") || containsSeq(a, "-O", "-") || containsSeq(a, "-O", "/dev/null"), "downloads to a file"
	},
	"ping":     func(a []string) (bool, string) { return true, "" },
	"nc":       func(a []string) (bool, string) { return hasFlagLetter(a, 'z'), "opens a raw connection" },
	"ip":       ipOK,
	"ifconfig": func(a []string) (bool, string) { return len(nonFlags(a)) <= 1, "changes an interface" },
	"mount":    func(a []string) (bool, string) { return len(nonFlags(a)) == 0, "mounts a filesystem" },
	"dmesg": func(a []string) (bool, string) {
		return !hasFlag(a, "-c", "-C", "--clear", "--read-clear"), "clears the kernel log"
	},
	"tar":   func(a []string) (bool, string) { return tarListOnly(a), "extracts or creates an archive" },
	"unzip": func(a []string) (bool, string) { return hasFlag(a, "-l", "-t", "-v", "-Z"), "extracts an archive" },
	"gzip": func(a []string) (bool, string) {
		return hasFlag(a, "-l", "-t", "-c", "--list", "--test", "--stdout"), "compresses files"
	},
	"openssl": opensslOK,
	"timedatectl": func(a []string) (bool, string) {
		s := firstNonFlag(a)
		return s == "" || s == "status" || s == "show" || s == "timesync-status" || s == "show-timesync", "changes time settings"
	},
	"chronyc": func(a []string) (bool, string) {
		return oneOf(firstNonFlag(a), "", "tracking", "sources", "sourcestats", "activity", "ntpdata", "serverstats"), "changes chrony"
	},
	"ntpq":      func(a []string) (bool, string) { return true, "" },
	"systemctl": systemctlOK,
	"journalctl": func(a []string) (bool, string) {
		return !hasFlagPrefix(a, "--vacuum", "--rotate", "--flush", "--sync", "--relinquish-var", "--setup-keys"), "maintains the journal"
	},
	"service": func(a []string) (bool, string) {
		return len(a) == 0 || a[len(a)-1] == "status" || hasFlag(a, "--status-all"), "controls a service"
	},
	"launchctl": func(a []string) (bool, string) {
		return oneOf(firstNonFlag(a), "list", "print", "print-disabled", "blame", "version"), "controls launchd"
	},
	"git":     gitOK,
	"kubectl": kubectlOK,
	"helm": func(a []string) (bool, string) {
		return oneOf(firstNonFlag(a), "list", "ls", "status", "get", "history", "show", "version", "search", "env"), "changes a release"
	},
	"brew": func(a []string) (bool, string) {
		return oneOf(firstNonFlag(a), "list", "ls", "info", "--version", "outdated", "config", "doctor", "deps", "leaves", "search", "home"), "changes packages"
	},
	"apt": func(a []string) (bool, string) {
		return oneOf(firstNonFlag(a), "list", "show", "policy", "search", "depends", "rdepends"), "changes packages"
	},
	"apt-cache": func(a []string) (bool, string) { return true, "" },
	"dpkg": func(a []string) (bool, string) {
		return hasFlag(a, "-l", "-L", "-s", "-S", "--list", "--status", "--listfiles", "--search", "--get-selections"), "changes packages"
	},
	"rpm": func(a []string) (bool, string) {
		return len(a) > 0 && strings.HasPrefix(a[0], "-q"), "changes packages"
	},
	"go": func(a []string) (bool, string) {
		return oneOf(firstNonFlag(a), "version", "env", "list", "doc", "help"), "runs go " + firstNonFlag(a)
	},
	"cosmovisor": func(a []string) (bool, string) {
		return oneOf(firstNonFlag(a), "version", "config", "help", "show-upgrade-info"), "runs cosmovisor " + firstNonFlag(a)
	},
	"cast": func(a []string) (bool, string) {
		s := firstNonFlag(a)
		if s == "wallet" {
			return false, "cast wallet handles keys"
		}
		return oneOf(s, "call", "block", "block-number", "bn", "chain-id", "cid", "balance", "b", "tx", "t", "receipt", "re", "code", "storage", "nonce", "gas-price", "gp", "logs", "age", "client", "basefee", "estimate", "e", "to-hex", "to-dec", "to-wei", "from-wei", "abi-decode", "abi-encode", "calldata", "4byte", "sig", "keccak", "to-check-sum-address", "find-block", "run", "rpc", "decode-transaction"), "runs cast " + s
	},
	"docker": dockerOK,
	"docker-compose": func(a []string) (bool, string) {
		return composeOK(a), "changes containers (compose " + firstNonFlag(a) + ")"
	},
	"podman": dockerOK,
}

// wrappers recurse into an inner command (docker exec <c> <cmd…>).
var wrappers = map[string]func(*acc, []string){}

func init() {
	wrappers["docker"] = dockerWrap
	wrappers["podman"] = dockerWrap
	delete(checkers, "docker")
	delete(checkers, "podman")
}

func dockerWrap(a *acc, args []string) {
	rest := skipFlags(args, "--context", "-c", "-H", "--host", "--config", "-l", "--log-level")
	if len(rest) == 0 {
		return
	}
	sub := rest[0]
	if sub == "container" && len(rest) > 1 {
		sub, rest = rest[1], rest[1:]
	}
	switch sub {
	case "exec":
		inner := skipFlags(rest[1:], "-u", "--user", "-w", "--workdir", "-e", "--env", "--env-file", "--detach-keys")
		if len(inner) > 1 {
			a.simple(inner[1:]) // inner[0] is the container
		}
		return
	case "run":
		a.raise(toolkit.TierLocalChange, "starts a container (docker run)")
		inner := skipFlags(rest[1:], "-u", "--user", "-w", "--workdir", "-e", "--env", "--env-file", "-v", "--volume",
			"--name", "--network", "--net", "-p", "--publish", "--entrypoint", "--mount", "--tmpfs", "--platform", "-h", "--hostname", "--add-host", "--label", "-l", "--memory", "-m", "--cpus", "--restart", "--log-driver", "--log-opt", "--ulimit", "--cap-add", "--cap-drop", "--security-opt", "--device", "--pid", "--ipc", "--user-ns", "--userns", "--stop-signal", "--stop-timeout", "--health-cmd", "--pull", "--gpus", "--dns", "--expose", "--link", "--cidfile", "--group-add", "--shm-size", "--workdir")
		if len(inner) > 1 {
			a.simple(inner[1:]) // inner[0] is the image
		}
		return
	case "compose":
		if !composeOK(rest[1:]) {
			a.raise(toolkit.TierLocalChange, "changes containers (docker compose "+firstNonFlag(rest[1:])+")")
		}
		return
	}
	if ok, _ := dockerOK(rest); !ok {
		a.raise(toolkit.TierLocalChange, "changes docker state (docker "+sub+")")
	}
}

func dockerOK(a []string) (bool, string) {
	rest := skipFlags(a, "--context", "-c", "-H", "--host", "--config")
	if len(rest) == 0 {
		return true, ""
	}
	s := rest[0]
	if (s == "container" || s == "image" || s == "network" || s == "volume" || s == "system" || s == "context" || s == "buildx" || s == "node" || s == "service") && len(rest) > 1 {
		return oneOf(rest[1], "ls", "list", "inspect", "logs", "top", "port", "stats", "diff", "history", "df", "info", "show", "ps"), "changes docker state"
	}
	return oneOf(s, "ps", "images", "logs", "inspect", "stats", "top", "port", "diff", "version", "info", "history", "search", "events", "--version", "-v"), "changes docker state"
}

func composeOK(a []string) bool {
	rest := skipFlags(a, "-f", "--file", "-p", "--project-name", "--project-directory", "--env-file", "--profile", "--ansi", "--progress")
	return len(rest) == 0 || oneOf(rest[0], "ps", "logs", "config", "ls", "images", "top", "version", "port", "events")
}

func systemctlOK(a []string) (bool, string) {
	s := firstNonFlag(a)
	return s == "" || oneOf(s, "status", "is-active", "is-enabled", "is-failed", "list-units", "list-unit-files",
		"list-timers", "list-sockets", "list-jobs", "show", "cat", "list-dependencies", "show-environment", "get-default"), "changes services (systemctl " + s + ")"
}

func gitOK(a []string) (bool, string) {
	rest := skipFlags(a, "-C", "-c", "--git-dir", "--work-tree")
	if len(rest) == 0 {
		return true, ""
	}
	s, args := rest[0], rest[1:]
	why := "changes the repo (git " + s + ")"
	switch s {
	case "status", "log", "diff", "show", "rev-parse", "describe", "blame", "ls-files", "ls-tree", "ls-remote",
		"shortlog", "reflog", "grep", "cat-file", "show-ref", "rev-list", "whatchanged", "name-rev", "merge-base", "version", "help":
		return true, ""
	case "branch":
		return len(nonFlags(args)) == 0 && !hasFlag(args, "-d", "-D", "-m", "-M", "-c", "-C", "--delete", "--move", "--copy", "-u", "--set-upstream-to", "--unset-upstream"), why
	case "tag":
		return len(nonFlags(args)) == 0 || hasFlag(args, "-l", "--list"), why
	case "remote":
		return len(args) == 0 || args[0] == "-v" || args[0] == "show" || args[0] == "get-url", why
	case "config":
		return hasFlag(args, "--get", "--list", "-l", "--get-all", "--get-regexp"), why
	case "stash":
		return len(args) > 0 && (args[0] == "list" || args[0] == "show"), why
	case "worktree":
		return len(args) > 0 && args[0] == "list", why
	}
	return false, why
}

func kubectlOK(a []string) (bool, string) {
	rest := skipFlags(a, "-n", "--namespace", "--context", "--kubeconfig", "-l", "--selector", "-o", "--output", "--cluster", "--user", "-s", "--server")
	s := firstNonFlag(rest)
	why := "changes the cluster (kubectl " + s + ")"
	switch s {
	case "get", "describe", "logs", "top", "explain", "version", "api-resources", "api-versions", "cluster-info", "events", "":
		return true, ""
	case "config":
		return oneOf(firstNonFlag(rest[1:]), "view", "get-contexts", "current-context", "get-clusters", "get-users"), why
	case "auth":
		return firstNonFlag(rest[1:]) == "can-i", why
	case "rollout":
		return oneOf(firstNonFlag(rest[1:]), "status", "history"), why
	}
	return false, why
}

func findOK(a []string) (bool, string) {
	for _, x := range a {
		switch x {
		case "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls":
			return false, "find " + x + " runs or changes things"
		}
	}
	return true, ""
}

var awkWriteRe = regexp.MustCompile(`system\s*\(|print[f]?[^;}]*>\s*"|\|\s*"|getline\s*<`)

func awkOK(a []string) (bool, string) {
	for _, x := range a {
		if awkWriteRe.MatchString(x) {
			return false, "awk program writes files or runs commands"
		}
	}
	return !hasFlag(a, "-i"), "awk edits in place"
}

var readRPCRe = regexp.MustCompile(`"method"\s*:\s*"(eth_(get|block|call|chainId|syncing|gasPrice|estimateGas|feeHistory|maxPriority|protocolVersion|accounts|coinbase|mining|hashrate)[A-Za-z]*|net_[A-Za-z]+|web3_[A-Za-z]+|txpool_(status|inspect|content)|debug_trace[A-Za-z]*|status|net_info|health|abci_info|abci_query|block[a-z_]*|validators|consensus_state|dump_consensus_state|genesis[a-z_]*|tx|tx_search|commit|num_unconfirmed_txs|unconfirmed_txs)"`)

func curlOK(a []string) (bool, string) {
	write := false
	data := ""
	for i, x := range a {
		switch {
		case x == "-o" || x == "--output":
			if i+1 < len(a) && a[i+1] != "/dev/null" && a[i+1] != "-" {
				return false, "curl writes a file"
			}
		case x == "-O" || x == "--remote-name" || x == "-T" || strings.HasPrefix(x, "--upload-file") || x == "-K" || x == "--config":
			return false, "curl writes or uploads files"
		case x == "-X" || x == "--request":
			if i+1 < len(a) && !oneOf(strings.ToUpper(a[i+1]), "GET", "HEAD", "POST") {
				return false, "curl " + a[i+1] + " request"
			}
		case x == "-d" || strings.HasPrefix(x, "--data") || x == "--json" || x == "-F" || x == "--form":
			write = true
			if i+1 < len(a) {
				data += a[i+1]
			}
		case strings.HasPrefix(x, "-d") && len(x) > 2:
			write = true
			data += x[2:]
		}
	}
	if write {
		// JSON-RPC queries (eth_blockNumber, status, …) are reads
		if readRPCRe.MatchString(data) {
			return true, ""
		}
		return false, "curl sends data"
	}
	return true, ""
}

func ipOK(a []string) (bool, string) {
	rest := skipFlags(a, "-4", "-6", "-o", "-br", "-c", "-d", "-j", "-p", "-s")
	if len(rest) <= 1 {
		return true, ""
	}
	return oneOf(rest[1], "show", "list", "ls", "get"), "changes networking (ip " + strings.Join(firstN(rest, 2), " ") + ")"
}

func opensslOK(a []string) (bool, string) {
	s := firstNonFlag(a)
	if oneOf(s, "s_client", "version", "ciphers", "x509", "verify", "dgst", "speed", "list") {
		return !hasFlag(a, "-out"), "openssl writes a file"
	}
	return false, "runs openssl " + s
}

func tarListOnly(a []string) bool {
	for _, x := range a {
		if x == "--list" || x == "-t" {
			return true
		}
		if !strings.HasPrefix(x, "--") && strings.TrimLeft(x, "-") != "" && strings.ContainsRune(strings.TrimLeft(x, "-"), 't') && !strings.ContainsAny(x, "/.") {
			return true
		}
	}
	return false
}

func sedInPlace(a []string) bool {
	for _, x := range a {
		if x == "--in-place" || strings.HasPrefix(x, "--in-place=") {
			return true
		}
		if strings.HasPrefix(x, "-") && !strings.HasPrefix(x, "--") && len(x) > 1 && strings.ContainsRune(x[1:], 'i') {
			return true
		}
	}
	return false
}

// --- helpers -------------------------------------------------------------

func setOf(s string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(s) {
		m[f] = true
	}
	return m
}

func oneOf(s string, opts ...string) bool {
	for _, o := range opts {
		if s == o {
			return true
		}
	}
	return false
}

func hasFlag(a []string, flags ...string) bool {
	for _, x := range a {
		if oneOf(x, flags...) {
			return true
		}
	}
	return false
}

func hasFlagPrefix(a []string, prefixes ...string) bool {
	for _, x := range a {
		for _, p := range prefixes {
			if strings.HasPrefix(x, p) {
				return true
			}
		}
	}
	return false
}

func hasFlagLetter(a []string, l rune) bool {
	for _, x := range a {
		if strings.HasPrefix(x, "-") && !strings.HasPrefix(x, "--") && strings.ContainsRune(x[1:], l) {
			return true
		}
	}
	return false
}

func containsWord(a []string, sub string) bool {
	for _, x := range a {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

func containsSeq(a []string, x, y string) bool {
	for i := 0; i+1 < len(a); i++ {
		if a[i] == x && a[i+1] == y {
			return true
		}
	}
	return false
}

func allAre(a []string, v string) bool {
	for _, x := range a {
		if x != v {
			return false
		}
	}
	return true
}

func nonFlags(a []string) []string {
	var out []string
	for _, x := range a {
		if !strings.HasPrefix(x, "-") {
			out = append(out, x)
		}
	}
	return out
}

func firstNonFlag(a []string) string {
	for _, x := range a {
		if !strings.HasPrefix(x, "-") {
			return x
		}
	}
	return ""
}

func firstN(a []string, n int) []string {
	if len(a) < n {
		return a
	}
	return a[:n]
}

func indexOf(a []string, v string) int {
	for i, x := range a {
		if x == v {
			return i
		}
	}
	return -1
}

// skipFlags drops leading flags; those listed in withValue consume the
// next word as their value (unless written as --flag=value).
func skipFlags(a []string, withValue ...string) []string {
	i := 0
	for i < len(a) && strings.HasPrefix(a[i], "-") && a[i] != "-" {
		f := a[i]
		i++
		if f == "--" {
			break
		}
		if !strings.Contains(f, "=") && oneOf(f, withValue...) && i < len(a) {
			i++
		}
	}
	return a[i:]
}

// stripComments blanks shell comment lines and trailing " # …" comments
// so prose in a script never reads as a command.
func stripComments(b []byte) []byte {
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") {
			lines[i] = ""
			continue
		}
		if j := strings.Index(l, " #"); j >= 0 && !strings.ContainsAny(l[:j], `'"`) {
			lines[i] = l[:j]
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// lastNonFlag is the last argument that isn't a flag (a copy's target).
func lastNonFlag(a []string) string {
	for i := len(a) - 1; i >= 0; i-- {
		if !strings.HasPrefix(a[i], "-") {
			return a[i]
		}
	}
	return ""
}
