package upgradetool

// The governed-upgrade flow for docker-run nodes, in the order an operator
// runs it:
//
//	upgrade.releases  which releases are newer than what's running
//	upgrade.build     build the target image on the node, from source
//	upgrade.ready     the new image registers the plan's handler and the
//	                  running binary doesn't — gov.propose checks this too
//	upgrade.switch    after the halt: swap the image in the deployed
//	                  compose file and recreate the container
//
// The plan name is the dangerous part. A name the new binary doesn't
// register halts the chain for good; a name the running binary already
// registers is applied by the old binary itself and nothing switches. So
// both are checked against the binaries themselves, not the source.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/common"
)

// RegisterFlow adds the docker upgrade-flow tools.
func RegisterFlow(r *toolkit.Registry) {
	r.Register(releasesTool{})
	r.Register(buildTool{})
	r.Register(readyTool{})
	r.Register(switchTool{})
}

// --- the running container ---------------------------------------------------

// running is what the node's container runs and how it was deployed.
type running struct {
	Image, Version, Repo string // image ref; image version label; source repo (owner/name)
	ComposeFile          string // the deployed compose file (from compose labels)
	ComposeDir           string
	Binary               string // binary path inside the image
}

func dockerOnly(c *toolkit.Context) error {
	if c.Profile == nil || c.Profile.Service.Type != "docker" || c.Profile.Service.Unit == "" {
		return fmt.Errorf("this upgrade flow is for docker nodes (service.type docker, service.unit set)")
	}
	return nil
}

func sh(c *toolkit.Context, script string, limit time.Duration) (string, int, error) {
	h, err := c.Host()
	if err != nil {
		return "", -1, err
	}
	sub, cancel := toolkit.WithDeadline(c, limit)
	defer cancel()
	res, err := host.Exec(sub, h, script, 4<<20)
	c.LogShell(script, res.Code)
	return res.Output, res.Code, err
}

func inspectRunning(c *toolkit.Context) (*running, error) {
	if err := dockerOnly(c); err != nil {
		return nil, err
	}
	unit := common.ShellQ(c.Profile.Service.Unit)
	out, code, err := sh(c, "docker inspect -f '{{.Config.Image}}|{{json .Config.Labels}}' "+unit, time.Minute)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("docker inspect %s: %v %s", c.Profile.Service.Unit, err, strings.TrimSpace(out))
	}
	img, labelsJSON, _ := strings.Cut(strings.TrimSpace(out), "|")
	var labels map[string]string
	_ = json.Unmarshal([]byte(labelsJSON), &labels)
	r := &running{Image: img, Version: labels["org.opencontainers.image.version"], Binary: "/evmd/evmd"}
	if src := labels["org.opencontainers.image.source"]; strings.Contains(src, "github.com/") {
		_, r.Repo, _ = strings.Cut(src, "github.com/")
		r.Repo = strings.TrimSuffix(strings.TrimSuffix(r.Repo, "/"), ".git")
	}
	if f := labels["com.docker.compose.project.config_files"]; f != "" {
		r.ComposeFile = strings.Split(f, ",")[0]
		r.ComposeDir = labels["com.docker.compose.project.working_dir"]
	}
	if bin := c.Profile.Binary; bin != "" {
		r.Binary = "/evmd/" + bin
	}
	if r.Version == "" {
		if _, tag, ok := strings.Cut(img, ":"); ok {
			r.Version = tag
		}
	}
	return r, nil
}

// --- upgrade.releases ---------------------------------------------------------

type releasesTool struct{}

func (releasesTool) Name() string { return "upgrade.releases" }
func (releasesTool) Desc() string {
	return "Releases newer than what the node runs (version and source repo read from the running image), oldest first, " +
		"with the upgrade handler names each one registers in evmd/upgrades.go. Pick the target before upgrade.build."
}
func (releasesTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"repo": toolkit.Str("github owner/repo (default: the image's source label, or metadata.repo)"),
	})
}
func (releasesTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (releasesTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	cur := ""
	repo := a.String("repo", "")
	if r, err := inspectRunning(c); err == nil {
		cur = r.Version
		if repo == "" {
			repo = r.Repo
		}
	}
	if repo == "" && c.Profile != nil {
		repo = c.Profile.Metadata["repo"]
	}
	if repo == "" {
		return nil, fmt.Errorf("no source repo: the image has no org.opencontainers.image.source label — pass repo")
	}
	tags, err := listReleases(c, repo)
	if err != nil {
		return nil, err
	}
	var newer []string
	for _, t := range tags {
		if cur == "" || semverLess(cur, t) {
			newer = append(newer, t)
		}
	}
	sort.Slice(newer, func(i, j int) bool { return semverLess(newer[i], newer[j]) })
	var b strings.Builder
	fmt.Fprintf(&b, "running %s (%s)\n", orNone(cur), repo)
	if len(newer) == 0 {
		b.WriteString("no newer release\n")
		return &toolkit.Result{Text: b.String(), Data: map[string]any{"current": cur, "repo": repo, "releases": newer}}, nil
	}
	handlers := map[string][]string{}
	for _, t := range newer {
		handlers[t] = upgradeNames(c, repo, t)
		fmt.Fprintf(&b, "  %-10s registers %s\n", t, orNone(strings.Join(handlers[t], ", ")))
	}
	curHandlers := upgradeNames(c, repo, cur)
	if len(curHandlers) > 0 {
		fmt.Fprintf(&b, "running %s registers %s — a plan with that name would be applied by the old binary itself\n", cur, strings.Join(curHandlers, ", "))
	}
	data := map[string]any{"current": cur, "repo": repo, "releases": newer, "handlers": handlers, "current_handlers": curHandlers}
	if len(newer) > 1 && c.Chooser != nil {
		if i, err := c.Chooser(c, fmt.Sprintf("%d releases are newer than %s — which one is the target?", len(newer), orNone(cur)), newer); err == nil {
			data["chosen"] = newer[i]
			fmt.Fprintf(&b, "target: %s\n", newer[i])
		} else {
			// no one to ask: list them and leave the choice to the operator
			b.WriteString("several candidates: confirm the target with the operator before building\n")
		}
	} else if len(newer) == 1 {
		data["chosen"] = newer[0]
	} else {
		b.WriteString("several candidates: confirm the target with the operator before building\n")
	}
	return &toolkit.Result{Text: b.String(), Data: data}, nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func listReleases(c *toolkit.Context, repo string) ([]string, error) {
	req, err := http.NewRequestWithContext(c, "GET", "https://api.github.com/repos/"+repo+"/releases?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github releases for %s: %s", repo, resp.Status)
	}
	var rels []struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rels {
		if !r.Draft && !r.Prerelease && semverRe.MatchString(r.Tag) {
			out = append(out, r.Tag)
		}
	}
	return out, nil
}

var (
	semverRe      = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)
	upgradeNameRe = regexp.MustCompile(`(?m)^\s*(?:const\s+)?\w*UpgradeName\w*\s*=\s*"([^"]+)"`)
	setHandlerRe  = regexp.MustCompile(`SetUpgradeHandler\(\s*"([^"]+)"`)
)

func semverLess(a, b string) bool {
	pa, pb := semverRe.FindStringSubmatch(a), semverRe.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return a < b
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return false
}

// upgradeNames reads the handler names a tag registers in evmd/upgrades.go.
func upgradeNames(c *toolkit.Context, repo, tag string) []string {
	if tag == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(c, "GET", "https://raw.githubusercontent.com/"+repo+"/"+tag+"/evmd/upgrades.go", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	seen := map[string]bool{}
	var out []string
	for _, re := range []*regexp.Regexp{upgradeNameRe, setHandlerRe} {
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	return out
}

// --- upgrade.build ------------------------------------------------------------

type buildTool struct{}

func (buildTool) Name() string { return "upgrade.build" }
func (buildTool) Desc() string {
	return "Build the target release's image on the node from source, with the node's own Dockerfile (the one that built the " +
		"running image). Optionally adds a no-op upgrade handler (RunMigrations only) when the release registers none for this " +
		"upgrade; checks the built binary's version and handler. Run it on every node before proposing."
}
func (buildTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"tag":        toolkit.Str("release tag to build, e.g. v0.6.1"),
		"image":      toolkit.Str("image to produce; {tag} is the release tag, {version} the tag without v — e.g. primium-{tag} → primium-v0.7.2 (default: the profile's metadata.image, else the running image's name with the new tag)"),
		"dockerfile": toolkit.Str("Dockerfile on the node (default: Dockerfile.primium beside the compose project, or one level up)"),
		"handler":    toolkit.Str("forks only: add a no-op upgrade handler with this plan name (official releases ship their own — leave empty)"),
		"repo":       toolkit.Str("github owner/repo (default: the running image's source)"),
	}, "tag")
}
func (buildTool) Tier() toolkit.Tier                 { return toolkit.TierLocalChange }
func (buildTool) Timeout(toolkit.Args) time.Duration { return 50 * time.Minute }

var tagRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (buildTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	r, err := inspectRunning(c)
	if err != nil {
		return nil, err
	}
	tag := a.String("tag", "")
	handler := a.String("handler", "")
	repo := a.String("repo", r.Repo)
	if !tagRe.MatchString(tag) || (handler != "" && !tagRe.MatchString(handler)) || repo == "" {
		return nil, fmt.Errorf("tag, handler and repo must be plain names (letters, digits, . _ -)")
	}
	image := ImageFor(c, a.String("image", ""), r.Image, tag)
	if image == r.Image {
		return nil, fmt.Errorf("%s is the image running now — build a different tag", image)
	}
	dockerfile := a.String("dockerfile", "")
	if dockerfile == "" && r.ComposeDir != "" {
		for _, d := range []string{r.ComposeDir, path.Dir(r.ComposeDir)} {
			p := path.Join(d, "Dockerfile.primium")
			if _, code, _ := sh(c, "test -f "+common.ShellQ(p), time.Minute); code == 0 {
				dockerfile = p
				break
			}
		}
	}
	if dockerfile == "" {
		// deployments differ per host: look under the user's home
		out, _, _ := sh(c, "find \"$HOME\" -maxdepth 7 -name Dockerfile.primium -not -path '*/.cometcli-build/*' 2>/dev/null | head -10", 2*time.Minute)
		var found []string
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				found = append(found, l)
			}
		}
		switch len(found) {
		case 1:
			dockerfile = found[0]
		case 0:
			return nil, fmt.Errorf("no Dockerfile.primium beside the compose project or under the home dir — pass dockerfile (the one that built %s)", r.Image)
		default:
			return nil, fmt.Errorf("several Dockerfile.primium on this host — pass dockerfile with the one that built %s:\n  %s", r.Image, strings.Join(found, "\n  "))
		}
	}
	src := "$HOME/.cometcli-build/" + path.Base(repo) + "-" + tag
	var script strings.Builder
	fmt.Fprintf(&script, "set -e\nsrc=%s\nrm -rf \"$src\" && mkdir -p \"$(dirname \"$src\")\"\n", src)
	fmt.Fprintf(&script, "git clone -q --depth 1 --branch %s https://github.com/%s.git \"$src\"\ncd \"$src\"\n", common.ShellQ(tag), repo)
	fmt.Fprintf(&script, "git fetch -q --depth 1 origin 'refs/tags/*:refs/tags/*' || true\n")
	if handler != "" {
		// a no-op handler next to the release's own: the plan only
		// switches binaries, migrations run for whatever modules changed
		fmt.Fprintf(&script, `f=evmd/upgrades.go
grep -q 'func (app EVMD) RegisterUpgradeHandlers() {' "$f" || { echo "evmd/upgrades.go has no RegisterUpgradeHandlers to extend"; exit 3; }
if ! grep -q '"%[1]s"' "$f"; then
  sed -i 's|func (app EVMD) RegisterUpgradeHandlers() {|func (app EVMD) RegisterUpgradeHandlers() {\n\tapp.UpgradeKeeper.SetUpgradeHandler("%[1]s", func(ctx context.Context, _ upgradetypes.Plan, fromVM module.VersionMap) (module.VersionMap, error) {\n\t\treturn app.ModuleManager.RunMigrations(ctx, app.Configurator(), fromVM)\n\t})|' "$f"
  echo "added upgrade handler %[1]s"
fi
`, handler)
	}
	fmt.Fprintf(&script, "docker build -q -f %s -t %s --label name=primium --label org.opencontainers.image.version=%s --label org.opencontainers.image.source=https://github.com/%s . >/dev/null\n",
		common.ShellQ(dockerfile), common.ShellQ(image), common.ShellQ(tag), repo)
	fmt.Fprintf(&script, "echo \"version: $(docker run --rm --entrypoint %s %s version 2>&1 | head -1)\"\n", r.Binary, common.ShellQ(image))
	detail := map[string]any{"image": image, "tag": tag, "repo": repo, "dockerfile": dockerfile, "source": src}
	if handler != "" {
		detail["adds handler"] = handler
	}
	if err := c.Approve(fmt.Sprintf("build %s on %s from %s@%s (takes several minutes; the node keeps running)", image, c.Profile.Name, repo, tag), toolkit.TierLocalChange, detail); err != nil {
		return nil, err
	}
	if c.Progress != nil {
		c.Progress("building " + image + " — several minutes")
	}
	out, code, err := sh(c, script.String(), 45*time.Minute)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("build failed (exit %d): %v\n%s", code, err, tail(out, 30))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "built %s from %s@%s with %s\n%s", image, repo, tag, dockerfile, strings.TrimSpace(out))
	data := map[string]any{"image": image, "tag": tag}
	if handler != "" {
		ok, err := imageHas(c, image, r.Binary, handler)
		if err != nil || !ok {
			return nil, fmt.Errorf("built %s but its binary doesn't contain the handler %q — don't propose with it", image, handler)
		}
		fmt.Fprintf(&b, "\nhandler %s: in the new binary ✓", handler)
		data["handler"] = handler
	}
	return &toolkit.Result{Text: b.String(), Data: data}, nil
}

func tail(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// imageHas reports whether an image's binary contains the plan name.
func imageHas(c *toolkit.Context, image, binary, name string) (bool, error) {
	out, _, err := sh(c, fmt.Sprintf("docker run --rm --entrypoint sh %s -c 'grep -c -a -F %s %s || true'", common.ShellQ(image), common.ShellQ(name), binary), 2*time.Minute)
	if err != nil {
		return false, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(lastLine(out)))
	return n > 0, nil
}

// runningHas reports whether the running container's binary contains it.
func runningHas(c *toolkit.Context, binary, name string) (bool, error) {
	out, _, err := sh(c, fmt.Sprintf("docker exec %s sh -c 'grep -c -a -F %s %s || true'", common.ShellQ(c.Profile.Service.Unit), common.ShellQ(name), binary), time.Minute)
	if err != nil {
		return false, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(lastLine(out)))
	return n > 0, nil
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return l[len(l)-1]
}

// --- upgrade.ready ------------------------------------------------------------

type readyTool struct{}

func (readyTool) Name() string { return "upgrade.ready" }
func (readyTool) Desc() string {
	return "Is this node ready for a governed upgrade to `image` under plan `name`? The new image must exist and its binary " +
		"register the name; the running binary must NOT (it would apply the plan itself and nothing switches); the deployed " +
		"compose file must be found. Run on every validator before proposing."
}
func (readyTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"name":  toolkit.Str("upgrade plan name"),
		"image": toolkit.Str("the image to switch to after the halt"),
	}, "name", "image")
}
func (readyTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (readyTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	ok, text, data, err := Ready(c, a.String("name", ""), a.String("image", ""))
	if err != nil {
		return nil, err
	}
	data["ready"] = ok
	return &toolkit.Result{Text: text, Data: data}, nil
}

// Ready checks one node for a governed upgrade (see upgrade.ready).
func Ready(c *toolkit.Context, name, image string) (bool, string, map[string]any, error) {
	r, err := inspectRunning(c)
	if err != nil {
		return false, "", nil, err
	}
	if !tagRe.MatchString(name) || image == "" {
		return false, "", nil, fmt.Errorf("name and image are required")
	}
	var b strings.Builder
	ok := true
	check := func(pass bool, line string) {
		mark := "✓"
		if !pass {
			mark, ok = "✗", false
		}
		fmt.Fprintf(&b, "%s %s\n", mark, line)
	}
	_, code, _ := sh(c, "docker image inspect "+common.ShellQ(image)+" >/dev/null 2>&1", time.Minute)
	check(code == 0, "image "+image+" is on the node")
	if code == 0 {
		has, err := imageHas(c, image, r.Binary, name)
		check(err == nil && has, fmt.Sprintf("the new binary registers %q", name))
	}
	old, err := runningHas(c, r.Binary, name)
	check(err == nil && !old, fmt.Sprintf("the running binary (%s) does not register %q", r.Image, name))
	check(r.ComposeFile != "", "deployed compose file: "+orNone(r.ComposeFile))
	if ok {
		b.WriteString("ready — the old binary will halt at the plan height and this image can resume it\n")
	}
	return ok, b.String(), map[string]any{"running_image": r.Image, "compose_file": r.ComposeFile}, nil
}

// --- upgrade.switch -----------------------------------------------------------

type switchTool struct{}

func (switchTool) Name() string { return "upgrade.switch" }
func (switchTool) Desc() string {
	return "After the old binary halted at the upgrade height: set the new image in the node's deployed docker-compose file " +
		"and recreate the container (compose down, up -d) on the same data dir. Refuses before the halt."
}
func (switchTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{
		"image": toolkit.Str("the image to run now, e.g. primium-evm:v0.6.1"),
	}, "image")
}
func (switchTool) Tier() toolkit.Tier                 { return toolkit.TierLocalChange }
func (switchTool) Timeout(toolkit.Args) time.Duration { return 10 * time.Minute }

var upgradeNeededRe = regexp.MustCompile(`UPGRADE "([^"]+)" NEEDED at height: (\d+)`)

func (switchTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	r, err := inspectRunning(c)
	if err != nil {
		return nil, err
	}
	image := a.String("image", "")
	if image == "" || strings.ContainsAny(image, "'\"|\\ ") {
		return nil, fmt.Errorf("image is required (e.g. primium-evm:v0.6.1)")
	}
	if r.ComposeFile == "" {
		return nil, fmt.Errorf("%s isn't managed by docker compose — no deployed compose file to update", c.Profile.Service.Unit)
	}
	if r.Image == image {
		return nil, fmt.Errorf("%s already runs %s", c.Profile.Service.Unit, image)
	}
	// only after the halt: the old binary must stop itself at the height
	logs, _, _ := sh(c, "docker logs --tail 300 "+common.ShellQ(c.Profile.Service.Unit)+" 2>&1 | grep -a 'UPGRADE \"' | tail -1", time.Minute)
	m := upgradeNeededRe.FindStringSubmatch(logs)
	if m == nil {
		return nil, fmt.Errorf("%s hasn't halted for an upgrade (no `UPGRADE \"…\" NEEDED` in its log) — switching early runs the new binary on pre-upgrade state", c.Profile.Service.Unit)
	}
	if ok, err := imageHas(c, image, r.Binary, m[1]); err != nil || !ok {
		return nil, fmt.Errorf("%s doesn't register the halted plan %q — it can't resume this chain", image, m[1])
	}
	dir := r.ComposeDir
	if dir == "" {
		dir = path.Dir(r.ComposeFile)
	}
	script := fmt.Sprintf("set -e\ncp %[1]s %[1]s.pre-%[2]s\nsed -i 's|image: *%[3]s *$|image: %[4]s|' %[1]s\ngrep -q 'image: %[4]s' %[1]s\ncd %[5]s\ndocker compose -f %[1]s down\ndocker compose -f %[1]s up -d\n",
		r.ComposeFile, strings.ReplaceAll(path.Base(image), ":", "-"), regexp.QuoteMeta(r.Image), image, common.ShellQ(dir))
	detail := map[string]any{"compose": r.ComposeFile, "from": r.Image, "to": image, "halted": m[1] + " at " + m[2]}
	if err := c.Approve(fmt.Sprintf("switch %s from %s to %s (halted for %s at %s): edit %s, compose down + up -d", c.Profile.Service.Unit, r.Image, image, m[1], m[2], r.ComposeFile), toolkit.TierLocalChange, detail); err != nil {
		return nil, err
	}
	out, code, err := sh(c, script, 8*time.Minute)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("switch failed (exit %d): %v\n%s\nthe compose file was backed up beside it (.pre-*)", code, err, tail(out, 20))
	}
	return &toolkit.Result{Text: fmt.Sprintf("%s now runs %s (compose file %s; backup kept as .pre-%s)\nnext: wait.until synced, then in-consensus",
		c.Profile.Service.Unit, image, r.ComposeFile, strings.ReplaceAll(path.Base(image), ":", "-")),
		Data: map[string]any{"image": image, "plan": m[1], "height": m[2]}}, nil
}

// ImageFor names the image built from a release tag: an explicit template,
// else the profile's metadata.image, else the running image's name with
// the new tag. {tag} is the tag (v0.7.2), {version} the tag without v.
func ImageFor(c *toolkit.Context, template, runningImage, tag string) string {
	if template == "" && c.Profile != nil {
		template = c.Profile.Metadata["image"]
	}
	if template == "" {
		name, _, _ := strings.Cut(runningImage, ":")
		return name + ":" + tag
	}
	return strings.NewReplacer("{tag}", tag, "{version}", strings.TrimPrefix(tag, "v")).Replace(template)
}
