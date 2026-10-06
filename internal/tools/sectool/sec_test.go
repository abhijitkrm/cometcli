package sectool

import "testing"

func byPort(ls []listener) map[string]listener {
	m := map[string]listener{}
	for _, l := range ls {
		m[l.port] = l
	}
	return m
}

func TestParseDockerPorts(t *testing.T) {
	out := `1317/tcp -> 0.0.0.0:1327
1317/tcp -> [::]:1327
6060/tcp -> 127.0.0.1:6070
26657/tcp -> 0.0.0.0:26667
26657/tcp -> [::]:26667`
	ls := parseDockerPorts(out)
	if len(ls) != 3 {
		t.Fatalf("want 3 deduped mappings, got %+v", ls)
	}
	m := byPort(ls)
	if !m["26657"].public || !m["1317"].public || m["6060"].public {
		t.Fatalf("publicity wrong: %+v", m)
	}
	if m["26657"].bind != "0.0.0.0:26667 (container :26657)" {
		t.Fatalf("bind = %q", m["26657"].bind)
	}
}

func TestParseSockets(t *testing.T) {
	cases := map[string]struct {
		out        string
		port       string
		wantPublic bool
	}{
		"ss":        {"LISTEN 0 4096 0.0.0.0:26657 0.0.0.0:*", "26657", true},
		"ss local":  {"LISTEN 0 4096 127.0.0.1:9090 0.0.0.0:*", "9090", false},
		"ss v6":     {"LISTEN 0 4096 [::]:8545 [::]:*", "8545", true},
		"linux net": {"tcp 0 0 0.0.0.0:1317 0.0.0.0:* LISTEN", "1317", true},
		"bsd any":   {"tcp46      0      0  *.26657                *.*                    LISTEN", "26657", true},
		"bsd local": {"tcp4       0      0  127.0.0.1.6060         *.*                    LISTEN", "6060", false},
	}
	for name, c := range cases {
		m := byPort(parseSockets(c.out))
		l, ok := m[c.port]
		if !ok || l.public != c.wantPublic {
			t.Errorf("%s: got %+v (ok=%v), want port %s public=%v", name, l, ok, c.port, c.wantPublic)
		}
	}
	if len(parseSockets("garbage\n\n")) != 0 {
		t.Fatal("garbage must yield no listeners")
	}
}
