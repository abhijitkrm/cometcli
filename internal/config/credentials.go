package config

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

const credentialsFile = "credentials"

// Credential returns an API key saved by `cometcli config set-key`
// (~/.cometcli/credentials, mode 0600, NAME=value lines), or "".
// Environment variables always take precedence over this file.
func Credential(name string) string {
	p, err := Path(credentialsFile)
	if err != nil {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && strings.TrimSpace(k) == name {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// SetCredential saves (or, with an empty value, removes) a key.
func SetCredential(name, value string) error {
	if name == "" || strings.ContainsAny(name, "=\n ") || strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("invalid credential name or value")
	}
	p, err := Path(credentialsFile)
	if err != nil {
		return err
	}
	kv := map[string]string{}
	if raw, err := os.ReadFile(p); err == nil {
		for _, l := range strings.Split(string(raw), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
				kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	if value == "" {
		delete(kv, name)
	} else {
		kv[name] = value
	}
	names := make([]string, 0, len(kv))
	for k := range kv {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		fmt.Fprintf(&b, "%s=%s\n", k, kv[k])
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
