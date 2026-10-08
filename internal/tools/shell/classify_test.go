package shell

import (
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

const (
	read  = toolkit.TierDiagnose
	local = toolkit.TierLocalChange
	chain = toolkit.TierOnChain
	forb  = toolkit.Tier(-1) // forbidden
)

func TestClassify(t *testing.T) {
	scripts := map[string]string{
		"./upgrade/vote-upgrade.sh": "#!/bin/bash\ndocker exec \"$C\" evmd tx gov vote \"$ID\" yes --from val --yes\n",
		"./info.sh":                 "#!/bin/bash\ncurl -s localhost:26657/status | jq .\n",
		"generate-keys.sh":          "#!/bin/bash\nread -p 'Number of validators: ' N\ndocker run --rm img evmd keys add v --keyring-backend test\n",
	}
	o := Opts{Binary: "evmd", ReadScript: func(p string) ([]byte, bool) {
		b, ok := scripts[p]
		return []byte(b), ok
	}}
	cases := []struct {
		cmd  string
		want toolkit.Tier
	}{
		// plain reads
		{"ls -la /var/lib", read},
		{"df -h && free -m", read},
		{"tail -n 200 /var/log/syslog | grep -i error", read},
		{"journalctl -u evmd --since '10 min ago' --no-pager", read},
		{"systemctl status evmd", read},
		{"docker ps --format '{{.Names}}'", read},
		{"docker logs --tail 100 validator0 2>&1 | grep -i panic", read},
		{"docker compose -f docker-compose.yml ps", read},
		{"docker exec validator0 evmd status", read},
		{"docker exec -i validator0 evmd q staking validators -o json | jq '.validators | length'", read},
		{"curl -s localhost:26657/status | jq .result.sync_info", read},
		{`curl -s -X POST -H 'content-type: application/json' --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' localhost:8545`, read},
		{"evmd q bank balances cosmos1abc", read},
		{"evmd keys list --keyring-backend test", read},
		{"git status && git log --oneline -5", read},
		{"kubectl get pods -n chain", read},
		{"find . -name '*.toml' -newer genesis.json", read},
		{"sed -n '1,40p' config/app.toml", read},
		{"awk '$5 > 80 {print $6}' <(df -h)", read},
		{"echo $(hostname) $(uptime)", read},
		{"cat config.toml 2>/dev/null || echo missing", read},
		{"ps aux | grep [e]vmd", read},
		{"xxd -l 32 data/blockstore.db/000001.log", read},
		{"/usr/bin/ls -l", read},
		{"cd run-validator && ls", read},
		{"FOO=1 env | sort", read},
		{"ss -tlnp | grep 26656", read},
		{"tar -tzf snapshot.tar.gz | head", read},
		{"./info.sh", local},

		// host changes
		{"systemctl restart evmd", local},
		{"echo hi > notes.txt", local},
		{"echo hi >> /etc/hosts", local},
		{"sed -i 's/a/b/' config.toml", local},
		{"docker compose up -d", local},
		{"docker restart validator0", local},
		{"docker run --rm evmd:latest evmd version", local},
		{"rm -rf ./tmp", local},
		{"sudo apt install jq", local},
		{"curl -o /tmp/x https://example.com", local},
		{"curl -X DELETE localhost:9200/index", local},
		{"python3 script.py", local},
		{"awk '{print > \"out\"}' f", local},
		{"find . -name '*.log' -delete", local},
		{"cat > config.toml <<EOF\n[p2p]\npersistent_peers = \"a@b:26656\"\nEOF", local},
		{"bash -c 'systemctl stop evmd'", local},
		{"evmd start --home /data", local},
		{"evmd tx bank send a b 1uatom --generate-only", read},
		{"git commit -m x", local},
		{"tee config.toml < new.toml", local},
		{"kubectl delete pod x", local},

		// transactions — always ask
		{"evmd tx staking unjail --from ops --yes", chain},
		{"docker exec validator0 evmd tx gov vote 3 yes --from val", chain},
		{"docker run --rm img evmd tx bank send a b 1adex", chain},
		{"cast send 0xabc 'f()' --private-key $K", chain},
		{`curl -X POST --data '{"method":"eth_sendRawTransaction","params":["0x"]}' localhost:8545`, chain},
		{"./upgrade/vote-upgrade.sh 3 yes", chain},
		{"bash ./upgrade/vote-upgrade.sh", chain},
		{"echo ok && evmd tx slashing unjail --from v", chain},

		// never via the agent
		{"cat ~/.evmd/config/priv_validator_key.json", forb},
		{"cp config/node_key.json /tmp/", forb},
		{"cat .mnemonics/mnemonics.txt", forb},
		{"evmd keys export ops", forb},
		{"evmd keys add newkey", forb},
		{"evmd tendermint unsafe-reset-all --home /data", forb},
		{"evmd comet unsafe-reset-all", forb},
		{"docker exec c evmd unsafe-reset-all", forb},
		{"rm data/priv_validator_state.json", forb},
		{"echo '{}' > data/priv_validator_state.json", forb},
		{"base64 ~/.ssh/id_ed25519", forb},
		{"generate-keys.sh", forb},
	}
	for _, c := range cases {
		v := Classify(c.cmd, o)
		got := v.Tier
		if v.Forbidden != "" {
			got = forb
		}
		if got != c.want {
			t.Errorf("%q: got %s (%s%s), want %s", c.cmd, tierName(got), v.Reason, v.Forbidden, tierName(c.want))
		}
	}
}

func tierName(t toolkit.Tier) string {
	if t == forb {
		return "forbidden"
	}
	return t.String()
}

func TestPermOnlyMayNameKeyFiles(t *testing.T) {
	for _, cmd := range []string{"ls -l config/priv_validator_key.json", "chmod 600 config/priv_validator_key.json", "stat -c %a config/node_key.json"} {
		if v := Classify(cmd, Opts{}); v.Forbidden != "" {
			t.Errorf("%q forbidden: %s", cmd, v.Forbidden)
		}
	}
}

func TestScriptNotes(t *testing.T) {
	o := Opts{ReadScript: func(string) ([]byte, bool) { return []byte("read -p 'name: ' N\necho $N\n"), true }}
	v := Classify("./setup.sh", o)
	if v.Tier != local || len(v.Notes) == 0 || !strings.Contains(v.Notes[0], "prompt") {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestSegmentsForRules(t *testing.T) {
	v := Classify("cd /data && git status | head -5; FOO=1 ls -l", Opts{})
	want := []string{"cd /data", "git status", "head -5", "FOO=1 ls -l"}
	if strings.Join(v.Segments, "|") != strings.Join(want, "|") {
		t.Fatalf("segments = %q", v.Segments)
	}
}
