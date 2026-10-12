package node

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/netspec"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// setConfigTool edits one key of a node config file in place.
type setConfigTool struct{}

func (setConfigTool) Name() string { return "node.set-config" }
func (setConfigTool) Desc() string {
	return "Set one key in the node's config.toml / app.toml / client.toml in place — comments and everything else kept, " +
		"the result checked to still parse, the previous file kept as .cometcli-bak. Works on a stopped docker node too. " +
		"Restart the node afterwards for it to take effect."
}
func (setConfigTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"file":  toolkit.Enum("which file", "config.toml", "app.toml", "client.toml"),
		"key":   toolkit.Str("section.key, e.g. mempool.type, json-rpc.enable, or a top-level key like minimum-gas-prices"),
		"value": toolkit.Str(`the value: app, true, 0, 0.0001atest — strings are quoted for you; pass TOML for lists, e.g. ["*"]`),
	}, "file", "key", "value")
}
func (setConfigTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

var (
	tomlKeyRe = regexp.MustCompile(`^([A-Za-z0-9_.\-]+\.)?[A-Za-z0-9_\-]+$`)
	tomlLitRe = regexp.MustCompile(`^(true|false|-?\d+(\.\d+)?|".*"|'.*'|\[.*\])$`)
)

// tomlLiteral quotes a bare string value.
func tomlLiteral(v string) string {
	v = strings.TrimSpace(v)
	if tomlLitRe.MatchString(v) {
		return v
	}
	return strconv.Quote(v)
}

func (setConfigTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	file, key := a.String("file", ""), a.String("key", "")
	if file != "config.toml" && file != "app.toml" && file != "client.toml" {
		return nil, fmt.Errorf("file: config.toml, app.toml or client.toml")
	}
	if !tomlKeyRe.MatchString(key) {
		return nil, fmt.Errorf("key %q: want section.key or a top-level key", key)
	}
	section, name := "", key
	if i := strings.LastIndex(key, "."); i >= 0 {
		section, name = key[:i], key[i+1:]
	}
	value := tomlLiteral(a.String("value", ""))
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	raw, where, err := common.ReadNodeFile(c, h, "config/"+file)
	if err != nil {
		return nil, err
	}
	before := string(raw)
	var old map[string]any
	_, _ = toml.Decode(before, &old)
	prev := netspec.Lookup(old, strings.Split(key, "."))
	after := netspec.SetTOML(before, section, name, value)
	var check map[string]any
	if _, err := toml.Decode(after, &check); err != nil {
		return nil, fmt.Errorf("the result wouldn't parse (nothing written): %v", err)
	}
	if after == before {
		return &toolkit.Result{Text: fmt.Sprintf("%s already %s — nothing to change", key, value), Data: map[string]any{"changed": false}}, nil
	}
	if err := c.Approve(fmt.Sprintf("set %s %s = %s (was %v)", file, key, value, prev), toolkit.TierLocalChange,
		map[string]any{"diff": toolkit.Diff(where, before, after)}); err != nil {
		return nil, err
	}
	at, err := common.WriteNodeFile(c, h, "config/"+file, []byte(after), 0o644)
	if err != nil {
		return nil, err
	}
	return &toolkit.Result{Text: fmt.Sprintf("set %s = %s in %s (was %v; backup .cometcli-bak) — restart the node to apply", key, value, at, prev),
		Data: map[string]any{"changed": true, "previous": fmt.Sprint(prev)}}, nil
}

// recreateTool recreates a docker node from its own compose file.
type recreateTool struct{}

func (recreateTool) Name() string { return "node.recreate" }
func (recreateTool) Desc() string {
	return "Recreate the node's docker container from the compose file that created it (docker compose up -d --force-recreate " +
		"<service>): picks up compose and image changes and reattaches its networks. Same data dir; nothing re-initialised."
}
func (recreateTool) Schema() map[string]any { return toolkit.ObjSchema(nil) }
func (recreateTool) Tier() toolkit.Tier     { return toolkit.TierLocalChange }
func (recreateTool) Timeout(toolkit.Args) time.Duration {
	return 5 * time.Minute
}

func (recreateTool) Run(c *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
	p := c.Profile
	if p == nil || p.Service.Type != "docker" || p.Service.Unit == "" {
		return nil, fmt.Errorf("node.recreate is for docker nodes (service.type docker, service.unit)")
	}
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	out, code, err := h.Run(c, "docker inspect -f '{{index .Config.Labels \"com.docker.compose.project.config_files\"}}|{{index .Config.Labels \"com.docker.compose.project.working_dir\"}}|{{index .Config.Labels \"com.docker.compose.service\"}}' "+common.ShellQ(p.Service.Unit))
	parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
	if err != nil || code != 0 || len(parts) < 3 || parts[0] == "" || parts[0] == "<no value>" {
		return nil, fmt.Errorf("%s wasn't created by docker compose — no compose file to recreate it from", p.Service.Unit)
	}
	file, dir, svc := strings.Split(parts[0], ",")[0], parts[1], parts[2]
	if err := c.Approve(fmt.Sprintf("recreate %s (docker compose up -d --force-recreate %s, %s)", p.Service.Unit, svc, file), toolkit.TierLocalChange,
		map[string]any{"compose": file, "service": svc}); err != nil {
		return nil, err
	}
	res, err := host.Exec(c, h, fmt.Sprintf("cd %s && docker compose -f %s up -d --force-recreate %s 2>&1 | tail -3", common.ShellQ(dir), common.ShellQ(file), common.ShellQ(svc)), 64<<10)
	if err != nil || res.Code != 0 {
		return nil, fmt.Errorf("recreate failed: %v %s", err, strings.TrimSpace(res.Output))
	}
	return &toolkit.Result{Text: fmt.Sprintf("recreated %s from %s", p.Service.Unit, file), Data: map[string]any{"compose": file}}, nil
}

// repairConfigTool makes a config file that doesn't parse parse again.
type repairConfigTool struct{}

func (repairConfigTool) Name() string { return "node.repair-config" }
func (repairConfigTool) Desc() string {
	return "Repair a config.toml / app.toml / client.toml that doesn't parse: comment out the few lines the TOML parser " +
		"rejects (everything else kept), or else restore the .cometcli-bak copy cometcli kept at its last edit. Shows the diff " +
		"and asks; the broken file is kept as .cometcli-bak. Refuses when more than a few lines are broken."
}
func (repairConfigTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"file": toolkit.Enum("which file", "config.toml", "app.toml", "client.toml"),
	}, "file")
}
func (repairConfigTool) Tier() toolkit.Tier { return toolkit.TierLocalChange }

// maxRepairLines is how many broken lines a repair may disable: more is a
// file that needs a person, not a patch.
const maxRepairLines = 3

func parses(doc string) error {
	var m map[string]any
	_, err := toml.Decode(doc, &m)
	return err
}

// RepairTOML comments out the lines a TOML document fails to parse on,
// one at a time, up to max; it returns the repaired document and the
// lines it disabled.
func RepairTOML(doc string, max int) (string, []string, error) {
	var disabled []string
	for {
		err := parses(doc)
		if err == nil {
			return doc, disabled, nil
		}
		var pe toml.ParseError
		if !errors.As(err, &pe) || pe.Position.Line < 1 {
			return "", nil, err
		}
		if len(disabled) == max {
			return "", nil, fmt.Errorf("more than %d lines don't parse (still: line %d: %s)", max, pe.Position.Line, pe.Message)
		}
		lines := strings.Split(doc, "\n")
		i := pe.Position.Line - 1
		blank := func(i int) bool {
			t := strings.TrimSpace(lines[i])
			return t == "" || strings.HasPrefix(t, "#")
		}
		if i >= len(lines) {
			i = len(lines) - 1
		}
		// an error found at the end of a line is reported on the next one
		// ("[consensus" then a newline): step back to the line that has it
		for i > 0 && blank(i) {
			i--
		}
		if i < 0 || blank(i) {
			return "", nil, fmt.Errorf("line %d: %s", pe.Position.Line, pe.Message)
		}
		disabled = append(disabled, fmt.Sprintf("line %d: %s (%s)", i+1, strings.TrimSpace(lines[i]), pe.Message))
		lines[i] = "# cometcli: disabled, didn't parse: " + lines[i]
		doc = strings.Join(lines, "\n")
	}
}

func (repairConfigTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	file := a.String("file", "")
	if file != "config.toml" && file != "app.toml" && file != "client.toml" {
		return nil, fmt.Errorf("file: config.toml, app.toml or client.toml")
	}
	h, err := c.Host()
	if err != nil {
		return nil, err
	}
	rel := "config/" + file
	raw, where, err := common.ReadNodeFile(c, h, rel)
	if err != nil {
		return nil, err
	}
	before := string(raw)
	if parses(before) == nil {
		return &toolkit.Result{Text: file + " parses — nothing to repair", Data: map[string]any{"changed": false}}, nil
	}
	after, disabled, rerr := RepairTOML(before, maxRepairLines)
	how := ""
	if rerr == nil {
		how = fmt.Sprintf("comment out %d line(s) that don't parse: %s", len(disabled), strings.Join(disabled, "; "))
	} else if bak, _, err := common.ReadNodeFile(c, h, rel+".cometcli-bak"); err == nil && parses(string(bak)) == nil {
		after, how = string(bak), "restore the copy cometcli kept at its last edit (.cometcli-bak) — "+rerr.Error()
	} else {
		return nil, fmt.Errorf("%s can't be repaired automatically: %v — and there's no .cometcli-bak that parses", file, rerr)
	}
	if err := c.Approve(fmt.Sprintf("repair %s: %s", file, how), toolkit.TierLocalChange,
		map[string]any{"diff": toolkit.Diff(where, before, after)}); err != nil {
		return nil, err
	}
	at, err := common.WriteNodeFile(c, h, rel, []byte(after), 0o644)
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("repaired %s: %s (the broken file is kept as .cometcli-bak) — restart the node to apply", at, how)
	if len(disabled) > 0 {
		text += "; a disabled key falls back to its default until it's set again"
	}
	return &toolkit.Result{Text: text, Data: map[string]any{"changed": true, "disabled": disabled}}, nil
}
