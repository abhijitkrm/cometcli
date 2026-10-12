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
	_ "embed"
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

	upgradev1beta1 "cosmossdk.io/api/cosmos/upgrade/v1beta1"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/netspec"
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
		mark := ""
		if why := breaking(c, repo, t); why != "" {
			mark = "  [" + why + "]"
		}
		fmt.Fprintf(&b, "  %-10s registers %s%s\n", t, orNone(strings.Join(handlers[t], ", ")), mark)
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
		"image":      toolkit.Str("image to produce; {tag} is the release tag, {version} the tag without v — e.g. mychain-{tag} → mychain-v0.7.2 (default: the profile's metadata.image, else the running image's name with the new tag)"),
		"dockerfile": toolkit.Str("Dockerfile on the node (default: the one the last build used, a Dockerfile beside the node's compose project, else cometcli's built-in one; \"builtin\" forces that)"),
		"handler":    toolkit.Str("forks only: add a no-op upgrade handler with this plan name (official releases ship their own — leave empty)"),
		"repo":       toolkit.Str("github owner/repo (default: the running image's source)"),
	}, "tag")
}
func (buildTool) Tier() toolkit.Tier                 { return toolkit.TierLocalChange }
func (buildTool) Timeout(toolkit.Args) time.Duration { return 50 * time.Minute }

var tagRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (buildTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	asked := a.String("image", "") // before the spec's naming fills it in
	r, err := inspectRunning(c)
	fresh := false
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "no such") {
			return nil, err
		}
		// no container yet (a new host): an official release under the
		// naming asked for
		r, fresh = &running{Binary: "/evmd/evmd", Repo: "cosmos/evm"}, true
		if bin := c.Profile.Binary; bin != "" {
			r.Binary = "/evmd/" + bin
		}
		if repo := c.Profile.Metadata["repo"]; repo != "" {
			r.Repo = repo
		}
		specDefaults(c.Profile, r, a)
	} else if a.String("image", "") == "" && c.Profile.Metadata["image"] == "" {
		// the spec's naming beats deriving from the running image: with
		// mychain-{tag}, the version is in the name, not the tag
		specImage(c.Profile, a)
	}
	tag := a.String("tag", "")
	handler := a.String("handler", "")
	if handler == "" && tag != "" {
		// a rebuild must reproduce the image the chain ran: if this tag was
		// built with an added handler, add it again
		handler = c.Profile.Metadata["handler."+tag]
		if handler == "" && c.Profile.ChainID != "" {
			if spec, _, err := netspec.Load(c.Profile.ChainID); err == nil {
				handler = spec.HandlerFor(tag)
			}
		}
	}
	record, err := handlerRecord(c, asked, r.Image, tag, handler)
	if err != nil {
		return nil, err
	}
	repo := a.String("repo", r.Repo)
	if !tagRe.MatchString(tag) || (handler != "" && !tagRe.MatchString(handler)) || repo == "" {
		return nil, fmt.Errorf("tag, handler and repo must be plain names (letters, digits, . _ -)")
	}
	image := ImageFor(c, a.String("image", ""), r.Image, tag)
	if fresh && strings.HasPrefix(image, ":") {
		return nil, fmt.Errorf("%s has no node container yet to take the image name from — pass image, e.g. mychain-{tag}", c.Profile.Name)
	}
	if image == r.Image {
		return nil, fmt.Errorf("%s is the image running now — build a different tag", image)
	}
	dockerfile := a.String("dockerfile", "")
	if fresh && dockerfile == "" && c.Profile.Metadata["dockerfile"] == "" {
		// no node here yet, so no Dockerfile that built it: cometcli's own
		dockerfile = "builtin"
	}
	if remembered := c.Profile.Metadata["dockerfile"]; dockerfile == "" && remembered != "" {
		// the one the last build here used
		if _, code, _ := sh(c, "test -f "+common.ShellQ(remembered), time.Minute); code == 0 {
			dockerfile = remembered
		}
	}
	if dockerfile == "" && r.ComposeDir != "" {
		// the Dockerfile beside the node's compose project (or one level up)
		out, _, _ := sh(c, "for d in "+common.ShellQ(r.ComposeDir)+" "+common.ShellQ(path.Dir(r.ComposeDir))+
			"; do find \"$d\" -maxdepth 1 -type f -name 'Dockerfile*' ! -name '.cometcli.Dockerfile' 2>/dev/null; done | sort -u | head -10", time.Minute)
		var found []string
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				found = append(found, l)
			}
		}
		switch len(found) {
		case 0:
		case 1:
			dockerfile = found[0]
		default:
			return nil, fmt.Errorf("several Dockerfiles beside %s — pass dockerfile with the one that built %s (or \"builtin\" for cometcli's):\n  %s", r.ComposeDir, r.Image, strings.Join(found, "\n  "))
		}
	}
	if dockerfile == "" {
		dockerfile = "builtin"
	}
	builtinNote := ""
	if dockerfile == "builtin" {
		// none on this host: cometcli's own, written beside its builds
		home, _, _ := sh(c, "printf %s \"$HOME\"", time.Minute)
		h, err := c.Host()
		if err != nil {
			return nil, err
		}
		dockerfile = path.Join(strings.TrimSpace(home), ".cometcli-build", "Dockerfile.evmd")
		if _, code, _ := sh(c, "mkdir -p "+common.ShellQ(path.Dir(dockerfile)), time.Minute); code != 0 {
			return nil, fmt.Errorf("can't create %s on %s", path.Dir(dockerfile), c.Profile.Name)
		}
		if err := h.WriteFile(c, dockerfile, []byte(builtinDockerfile), 0o644); err != nil {
			return nil, err
		}
		why := "no Dockerfile beside the node's compose project"
		if fresh {
			why = "no node on this host yet"
		}
		builtinNote = "note: " + why + " — built with cometcli's built-in Dockerfile (" + dockerfile + ")\n"
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
	// preflight: the node's Dockerfile was written for the running release;
	// a newer release may need a newer Go than its builder image has —
	// build from a copy with the Go image raised (the original stays as is)
	fmt.Fprintf(&script, `df=%s
need=$(awk '/^toolchain go/{sub("go","",$2); print $2; exit} /^go [0-9]/{print $2; exit}' go.mod)
have=$(sed -n 's/^FROM golang:\([0-9][0-9.]*\).*/\1/p' "$df" | head -1)
if [ -n "$need" ] && [ -n "$have" ] && [ "$(printf '%%s\n%%s\n' "$have" "$need" | sort -V | head -1)" != "$need" ]; then
  sed "s/^FROM golang:$have/FROM golang:$need/" "$df" > .cometcli.Dockerfile
  df=.cometcli.Dockerfile
  echo "note: $(basename %s) builds with Go $have; $(git describe --tags 2>/dev/null) needs Go $need — built with golang:$need (your Dockerfile is unchanged)"
fi
`, common.ShellQ(dockerfile), common.ShellQ(dockerfile))
	fmt.Fprintf(&script, "docker build -q -f \"$df\" -t %s --label org.opencontainers.image.version=%s --label org.opencontainers.image.source=https://github.com/%s . >/dev/null\n",
		common.ShellQ(image), common.ShellQ(tag), repo)
	fmt.Fprintf(&script, "echo \"version: $(docker run --rm --entrypoint %s %s version 2>&1 | head -1)\"\n", r.Binary, common.ShellQ(image))
	detail := map[string]any{"image": image, "tag": tag, "repo": repo, "dockerfile": dockerfile, "source": src}
	if handler != "" {
		detail["adds handler"] = handler
	}
	// what the operator should know before saying yes
	if need, have := goNeeded(c, repo, tag), dockerfileGo(c, dockerfile); need != "" && have != "" && semverLess("v"+have, "v"+need) {
		detail["builder"] = fmt.Sprintf("Go %s → %s (%s needs it; the build uses a copy, your Dockerfile is unchanged)", have, need, tag)
	}
	prompt := fmt.Sprintf("build %s on %s from %s@%s (takes several minutes; the node keeps running)", image, c.Profile.Name, repo, tag)
	if why := breaking(c, repo, tag); why != "" && !fresh {
		// a running node could be switched onto it too early: the warning
		// matters, so it's always asked — a new host has nothing to protect
		detail["consensus"] = why + " — every validator must switch together at an upgrade height (gov proposal or halt-height); never restart a node onto it early"
		if err := toolkit.RequireApproval(c, "CONSENSUS-BREAKING release — "+prompt, toolkit.TierLocalChange, detail); err != nil {
			return nil, err
		}
	} else if err := c.Approve(prompt, toolkit.TierLocalChange, detail); err != nil {
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
	fmt.Fprintf(&b, "%sbuilt %s from %s@%s with %s\n%s", builtinNote, image, repo, tag, dockerfile, strings.TrimSpace(out))
	data := map[string]any{"image": image, "tag": tag}
	if handler != "" {
		ok, err := imageHas(c, image, r.Binary, handler)
		if err != nil || !ok {
			return nil, fmt.Errorf("built %s but its binary doesn't contain the handler %q — don't propose with it", image, handler)
		}
		fmt.Fprintf(&b, "\nhandler %s: in the new binary ✓", handler)
		data["handler"] = handler
	}
	// remember the Dockerfile, and any handler added to this tag, for the
	// next build of it on this node (a rebuild after the image was lost
	// must be the same binary, or the node can't replay the upgrade)
	if c.Cfg != nil && c.Profile.Name != "" {
		_ = c.Cfg.UpdateProfile(c.Profile.Name, func(p *config.Profile) {
			if p.Metadata == nil {
				p.Metadata = map[string]string{}
			}
			p.Metadata["dockerfile"] = dockerfile
			if record {
				p.Metadata["handler."+tag] = handler
			}
		})
		if record && c.Profile.ChainID != "" {
			if spec, _, err := netspec.Load(c.Profile.ChainID); err == nil && spec.RecordHandler(tag, handler) {
				_, _ = spec.Save()
			}
		}
		if record {
			if c.Profile.Metadata == nil {
				c.Profile.Metadata = map[string]string{}
			}
			c.Profile.Metadata["handler."+tag] = handler
		}
	}
	return &toolkit.Result{Text: b.String(), Data: data}, nil
}

// handlerRecord says whether a build of tag with handler is recorded for
// later rebuilds. The handler a tag was built with is what the chain ran:
// a build with another one is a different binary, so it needs its own
// image name and never replaces the record.
func handlerRecord(c *toolkit.Context, asked, running, tag, handler string) (bool, error) {
	if handler == "" {
		return false, nil
	}
	recorded := c.Profile.Metadata["handler."+tag]
	if recorded == "" && c.Profile.ChainID != "" {
		if spec, _, err := netspec.Load(c.Profile.ChainID); err == nil {
			recorded = spec.HandlerFor(tag)
		}
	}
	if recorded == "" || recorded == handler {
		return true, nil
	}
	named := toolkit.Args{}
	specImage(c.Profile, named)
	img := ImageFor(c, asked, running, tag)
	if asked == "" || img == ImageFor(c, "", running, tag) || img == ImageFor(c, named.String("image", ""), running, tag) {
		return false, fmt.Errorf("%s was built for this chain with handler %s — a rebuild of it must add the same one, or nodes replaying the upgrade diverge. To build %s with %s anyway, give it its own image name (image=…); it won't replace the record", tag, recorded, tag, handler)
	}
	return false, nil
}

// imagesFor lists the local images (other than the running one) whose
// binary registers plan; with several, those named the network's way win.
func imagesFor(c *toolkit.Context, r *running, plan string) []string {
	out, _, _ := sh(c, "docker images --format '{{.Repository}}:{{.Tag}}' | grep -v '<none>'", time.Minute)
	var found []string
	for _, img := range strings.Fields(out) {
		if img == r.Image {
			continue
		}
		if ok, err := imageHas(c, img, r.Binary, plan); err == nil && ok {
			found = append(found, img)
		}
	}
	if len(found) > 1 {
		if spec, _, err := netspec.Load(c.Profile.ChainID); err == nil && spec.Image.Naming != "" {
			re := regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(spec.Image.Naming), regexp.QuoteMeta("{tag}"), `v\d+\.\d+\.\d+`) + "(:latest)?$")
			var named []string
			for _, f := range found {
				if re.MatchString(f) {
					named = append(named, f)
				}
			}
			if len(named) == 1 {
				found = named
			}
		}
	}
	return found
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
		"name":  toolkit.Str("upgrade plan name (default: the plan scheduled on chain)"),
		"image": toolkit.Str("the image to switch to after the halt (default: the local image whose binary registers the plan)"),
	})
}
func (readyTool) Tier() toolkit.Tier { return toolkit.TierObserve }

func (readyTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	name, image := a.String("name", ""), a.String("image", "")
	if name == "" {
		g, err := c.GRPC()
		if err != nil {
			return nil, fmt.Errorf("pass name — can't read the scheduled plan: %w", err)
		}
		res, err := g.Upgrade.CurrentPlan(c, &upgradev1beta1.QueryCurrentPlanRequest{})
		if err != nil {
			return nil, err
		}
		if res.Plan == nil {
			return nil, fmt.Errorf("no upgrade is scheduled on chain — pass name (and image) to check ahead of a proposal")
		}
		name = res.Plan.Name
	}
	if image == "" {
		r, err := inspectRunning(c)
		if err != nil {
			return nil, err
		}
		switch found := imagesFor(c, r, name); len(found) {
		case 0:
			text := fmt.Sprintf("✗ no image on %s registers plan %q — build it before the halt: upgrade build --tag <release> (with --handler %s if the release has none)\nnot ready\n", c.Profile.Name, name, name)
			return &toolkit.Result{Text: text, Data: map[string]any{"ready": false, "name": name}}, nil
		case 1:
			image = found[0]
		default:
			return nil, fmt.Errorf("several images register %q (%s) — pass image", name, strings.Join(found, ", "))
		}
	}
	ok, text, data, err := Ready(c, name, image)
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
		"image": toolkit.Str("the image to run now, e.g. mychain-v0.7.3 (default: the local image whose binary registers the halted plan)"),
	})
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
	if strings.ContainsAny(image, "'\"|\\ ") {
		return nil, fmt.Errorf("image %q: a plain image name", image)
	}
	if r.ComposeFile == "" {
		return nil, fmt.Errorf("%s isn't managed by docker compose — no deployed compose file to update", c.Profile.Service.Unit)
	}
	if image == "" {
		// the image built for the halted plan: the one whose binary registers it
		logs, _, _ := sh(c, "docker logs --tail 300 "+common.ShellQ(c.Profile.Service.Unit)+" 2>&1 | grep -a 'UPGRADE \"' | tail -1", time.Minute)
		m := upgradeNeededRe.FindStringSubmatch(logs)
		if m == nil {
			return nil, fmt.Errorf("%s hasn't halted for an upgrade (no `UPGRADE \"…\" NEEDED` in its log) — nothing to switch to yet", c.Profile.Service.Unit)
		}
		found := imagesFor(c, r, m[1])
		switch len(found) {
		case 0:
			return nil, fmt.Errorf("no image on %s registers the halted plan %q — build it: upgrade build --tag <release> (and --handler %s if the release has none)", c.Profile.Name, m[1], m[1])
		case 1:
			image = found[0]
		default:
			return nil, fmt.Errorf("several images register %q (%s) — pass image", m[1], strings.Join(found, ", "))
		}
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

//go:embed builtin.Dockerfile
var builtinDockerfile string

var (
	breakingRe    = regexp.MustCompile(`(?i)\b(state|consensus)[ -]breaking\b`)
	notBreakingRe = regexp.MustCompile(`(?i)\bnon[ -](state|consensus)[ -]breaking\b|\bnot (state|consensus)[ -]breaking\b`)
	goLineRe      = regexp.MustCompile(`(?m)^toolchain go([0-9.]+)|^go ([0-9][0-9.]*)`)
	fromGoRe      = regexp.MustCompile(`(?m)^FROM golang:([0-9][0-9.]*)`)
)

// breaking says whether a release's notes call it state- or
// consensus-breaking ("" when they don't, or can't be read).
func breaking(c *toolkit.Context, repo, tag string) string {
	body := fetch(c, "https://api.github.com/repos/"+repo+"/releases/tags/"+tag)
	var rel struct {
		Body string `json:"body"`
	}
	if json.Unmarshal([]byte(body), &rel) != nil || notBreakingRe.MatchString(rel.Body) {
		return ""
	}
	if m := breakingRe.FindString(rel.Body); m != "" {
		return strings.ToLower(m) + " per the release notes"
	}
	return ""
}

// goNeeded is the Go version a release's go.mod asks for.
func goNeeded(c *toolkit.Context, repo, tag string) string {
	m := goLineRe.FindStringSubmatch(fetch(c, "https://raw.githubusercontent.com/"+repo+"/"+tag+"/go.mod"))
	switch {
	case m == nil:
		return ""
	case m[1] != "":
		return m[1]
	}
	return m[2]
}

// dockerfileGo is the Go version of the node Dockerfile's builder image.
func dockerfileGo(c *toolkit.Context, dockerfile string) string {
	out, _, err := sh(c, "cat "+common.ShellQ(dockerfile), time.Minute)
	if err != nil {
		return ""
	}
	if m := fromGoRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

func fetch(c *toolkit.Context, url string) string {
	req, err := http.NewRequestWithContext(c, "GET", url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(raw)
}

// specDefaults fills what a new host can't tell from a running node — the
// release's source repo and the image naming — from the network's spec.
func specDefaults(p *config.Profile, r *running, a toolkit.Args) {
	if p == nil || p.ChainID == "" {
		return
	}
	spec, _, err := netspec.Load(p.ChainID)
	if err != nil {
		return
	}
	if spec.Image.Repo != "" && p.Metadata["repo"] == "" {
		r.Repo = spec.Image.Repo
	}
	if a.String("image", "") == "" && p.Metadata["image"] == "" {
		specImage(p, a)
	}
}

// specImage sets the image naming from the network's spec, when it has one.
func specImage(p *config.Profile, a toolkit.Args) {
	if p == nil || p.ChainID == "" {
		return
	}
	spec, _, err := netspec.Load(p.ChainID)
	if err != nil {
		return
	}
	naming := spec.Image.Naming
	if naming == "" && spec.Image.Name != "" {
		naming = spec.Image.Name + ":{tag}"
	}
	if naming != "" {
		a["image"] = naming
	}
}
