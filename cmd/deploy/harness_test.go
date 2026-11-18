package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	testSystem    = "x86_64-linux"
	testApp       = "demo-app"
	flyToken      = "FlyV1 fm2_CANARY+1b2c/x=,y"
	porkbunKey    = "pk-CANARY-3d4e"
	porkbunSecret = "sk-CANARY-5f6a"
	oldDigest     = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
)

var (
	// deployBin is deploy built with toolDir as its pinned tool path and its
	// production verification budget, polling every 50ms. shortVerifyBin
	// differs only in a budget short enough for tests that wait it out.
	deployBin      string
	shortVerifyBin string
	// testStore stands in for the Nix store: the binaries are stamped with
	// it, and fixture build outputs live in it.
	testStore string
	toolDir   string
	realTools map[string]string
)

// ambientEnv is set on every deploy run: credentials and endpoint overrides
// an operator's shell might carry, each marked with ambientMarker. None of it
// may reach a child.
const ambientMarker = "AMBIENTCANARY"

var ambientEnv = []string{
	"FLY_API_TOKEN=AMBIENTCANARY-fly-token",
	"FLY_ACCESS_TOKEN=AMBIENTCANARY-fly-access",
	"FLY_APP=AMBIENTCANARY-app",
	"FLY_API_HOSTNAME=https://AMBIENTCANARY.invalid",
	"PORKBUN_API_KEY=AMBIENTCANARY-porkbun-key",
	"PORKBUN_SECRET_KEY=AMBIENTCANARY-porkbun-secret",
	"PORKBUN_API_BASE=http://AMBIENTCANARY.invalid",
	"REGISTRY_AUTH_FILE=/AMBIENTCANARY/auth.json",
	"DOCKER_CONFIG=/AMBIENTCANARY/docker",
	"NIX_CONFIG=access-tokens = github.com=AMBIENTCANARY",
	"GITHUB_TOKEN=AMBIENTCANARY-github",
	"GIT_CONFIG_PARAMETERS='credential.helper=AMBIENTCANARY'",
}

func TestMain(m *testing.M) {
	if code, ok := fakeTool(); ok {
		os.Exit(code)
	}
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "deploy-test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	toolDir = filepath.Join(dir, "bin")
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	if err := os.Mkdir(toolDir, 0o755); err != nil {
		panic(err)
	}
	for _, name := range append(fakeTools, "ignore-term") {
		if err := os.Symlink(self, filepath.Join(toolDir, name)); err != nil {
			panic(err)
		}
	}

	realTools = map[string]string{"dns": filepath.Join(dir, "dns-real")}
	mustBuild("go", "build", "-o", realTools["dns"], "gunk.dev/armstrong/cmd/dns")
	for _, name := range []string{"git", "cue"} {
		if p, err := exec.LookPath(name); err == nil {
			realTools[name] = p
		}
	}
	if realTools["git"] == "" {
		panic("the deploy tests need git on PATH")
	}
	if err := writeJSON(filepath.Join(toolDir, "real-tools.json"), realTools); err != nil {
		panic(err)
	}

	testStore = filepath.Join(dir, "store")
	if err := os.Mkdir(testStore, 0o755); err != nil {
		panic(err)
	}
	storeDir = testStore // for the tests that call deploy's code directly
	ldflags := strings.Join([]string{
		"-X main.storeDir=" + testStore,
		"-X main.version=test-version",
		"-X main.nixSystem=" + testSystem,
		"-X main.toolPath=" + toolDir,
		"-X main.verifyInterval=50ms",
	}, " ")
	deployBin = filepath.Join(dir, "deploy")
	mustBuild("go", "build", "-o", deployBin, "-ldflags", ldflags, ".")
	shortVerifyBin = filepath.Join(dir, "deploy-short-verify")
	mustBuild("go", "build", "-o", shortVerifyBin, "-ldflags", ldflags+" -X main.verifyTimeout=1s", ".")
	return m.Run()
}

func mustBuild(args ...string) {
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		panic(fmt.Sprintf("%v: %v\n%s", args, err, out))
	}
}

func requireCue(t *testing.T) {
	t.Helper()
	if realTools["cue"] == "" {
		t.Skip("cue not on PATH")
	}
}

// harness is one test's world: an infra repository, a state directory, the
// fakes' scenario and the fake Fly app.
type harness struct {
	t     *testing.T
	bin   string // the deploy binary to run
	root  string
	out   string // this test's build outputs, in the test store
	state string
	infra string
	rev   string
	sc    scenario
	world world
	// digest is the fixture image's manifest digest.
	digest string
}

func newHarness(t *testing.T) *harness {
	t.Parallel()
	root := t.TempDir()
	h := &harness{
		t:     t,
		bin:   deployBin,
		root:  root,
		state: filepath.Join(root, "state"),
		infra: filepath.Join(root, "infra"),
		sc:    scenario{System: testSystem, StoreDir: testStore, Outputs: map[string]fakeOutput{}},
	}
	out, err := os.MkdirTemp(testStore, "out-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(out) })
	h.out = out
	h.write("creds/fly-token", flyToken+"\n")
	h.write("creds/porkbun-key", porkbunKey+"\n")
	h.write("creds/porkbun-secret", porkbunSecret+"\r\n")
	h.gitIn(root, "init", "--quiet", h.infra)
	return h
}

func (h *harness) write(rel, content string) string {
	h.t.Helper()
	p := filepath.Join(h.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// writeOut writes a file under this test's build outputs.
func (h *harness) writeOut(rel, content string) string {
	h.t.Helper()
	p := filepath.Join(h.out, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) gitIn(dir string, args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+h.root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// baseFiles is the least an infra commit needs.
var baseFiles = map[string]string{
	"flake.nix":  "{ outputs = _: { }; }\n",
	"flake.lock": `{"nodes":{"root":{}},"root":"root","version":7}` + "\n",
	"marker":     "committed",
}

// commit writes files into the infra repository (an empty content deletes
// the file), commits everything and returns the commit.
func (h *harness) commit(files map[string]string) string {
	h.t.Helper()
	for name, content := range files {
		p := filepath.Join(h.infra, name)
		if content == "" {
			os.Remove(p)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	h.gitIn(h.infra, "add", "-A")
	h.gitIn(h.infra, "commit", "--quiet", "--allow-empty", "-m", "infra")
	return h.gitIn(h.infra, "rev-parse", "HEAD")
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// appFixture sets up app "demo": its config and an amd64 image as build
// outputs, and a Fly app with a started and a stopped Machine and one volume.
func (h *harness) appFixture() {
	h.t.Helper()
	cfg := h.writeOut("config/fly.json", `{"app":"demo-app","primary_region":"ord"}`)
	img := filepath.Join(h.out, "image.tar")
	h.digest = writeImage(h.t, img, image{arch: "amd64"})
	h.sc.Outputs["deploy-demo-config"] = fakeOutput{Path: filepath.Dir(cfg)}
	h.sc.Outputs["deploy-demo-image"] = fakeOutput{Path: img}
	h.world = world{
		App: testApp,
		Machines: []fakeMachine{
			{ID: "m2", State: "stopped", Digest: oldDigest},
			{ID: "m1", State: "started", Digest: oldDigest},
		},
		Volumes: []fakeVolume{{ID: "vol2"}, {ID: "vol1", Attached: "m1"}},
	}
	h.rev = h.commit(baseFiles)
}

// image describes a test OCI archive: one tiny image for arch, plus a second
// manifest in the index for twoPlatforms, as a multi-platform build would
// have, without the blob directory entries for noDirs, and with index.json
// twice for twoIndexes.
type image struct {
	arch         string
	twoPlatforms bool
	noDirs       bool
	twoIndexes   bool // a second index.json, naming another manifest, follows the first
}

// writeImage writes the archive and returns its first manifest's digest.
func writeImage(t *testing.T, path string, img image) string {
	t.Helper()
	blobs := map[string][]byte{}
	add := func(mediaType string, data []byte) descriptor {
		d := descriptor{MediaType: mediaType, Digest: "sha256:" + sha256Hex(data), Size: int64(len(data))}
		blobs[d.Digest] = data
		return d
	}
	manifestFor := func(arch string) descriptor {
		config := add("application/vnd.oci.image.config.v1+json", fmt.Appendf(nil, `{"architecture":%q,"os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`, arch))
		layer := add("application/vnd.oci.image.layer.v1.tar", []byte("layer for "+arch))
		m, _ := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": mediaOCIManifest,
			"config": config, "layers": []descriptor{layer},
		})
		return add(mediaOCIManifest, m)
	}
	manifests := []descriptor{manifestFor(img.arch)}
	if img.twoPlatforms {
		manifests = append(manifests, manifestFor("arm64"))
	}
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": manifests})

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	file := func(name string, data []byte) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
		tw.Write(data)
	}
	if !img.noDirs {
		for _, dir := range []string{"blobs/", "blobs/sha256/"} {
			tw.WriteHeader(&tar.Header{Name: dir, Mode: 0o755, Typeflag: tar.TypeDir})
		}
	}
	file("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	file("index.json", index)
	if img.twoIndexes {
		other, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []descriptor{manifestFor("amd64-other")}})
		file("index.json", other)
	}
	for _, d := range slices.Sorted(maps.Keys(blobs)) {
		file("blobs/sha256/"+strings.TrimPrefix(d, "sha256:"), blobs[d])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return manifests[0].Digest
}

// outcome is one finished deploy run.
type outcome struct {
	stdout, stderr string
	code           int
	recordPath     string
	rec            *record
}

// command prepares a deploy run: the scenario is rewritten every time, the
// fake world only when it does not exist yet, so it carries over between runs
// in one test.
func (h *harness) command(args ...string) *exec.Cmd {
	h.t.Helper()
	if err := writeJSON(filepath.Join(h.root, scenarioFile), h.sc); err != nil {
		h.t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "world.json")); errors.Is(err, os.ErrNotExist) {
		if err := writeJSON(filepath.Join(h.root, "world.json"), h.world); err != nil {
			h.t.Fatal(err)
		}
	}
	cmd := exec.Command(h.bin, args...)
	cmd.Env = append([]string{
		"DEPLOY_STATE_DIR=" + h.state,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(h.root, "home"),
	}, ambientEnv...)
	return cmd
}

func (h *harness) run(args ...string) outcome {
	h.t.Helper()
	cmd := h.command(args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	return h.finish(cmd.Run(), &stdout, &stderr)
}

func (h *harness) finish(err error, stdout, stderr *bytes.Buffer) outcome {
	h.t.Helper()
	o := outcome{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		o.code = exitErr.ExitCode()
	default:
		h.t.Fatalf("run deploy: %v", err)
	}
	if first, _, _ := strings.Cut(o.stdout, "\n"); strings.HasSuffix(first, "/record.json") {
		o.recordPath = first
		// A record that cannot be read leaves rec nil, for wantRun to report.
		var rec record
		if readJSON(first, &rec) == nil {
			o.rec = &rec
		}
	}
	for _, c := range h.calls() {
		if c.Info["unexpected"] != "" {
			h.t.Errorf("deploy made an unexpected call: %s %q", c.Tool, c.Args)
		}
	}
	return o
}

func (h *harness) appArgs(extra ...string) []string {
	return append([]string{"app", "demo", "--infra", h.infra, "--rev", h.rev,
		"--fly-token-file", filepath.Join(h.root, "creds", "fly-token")}, extra...)
}

func (h *harness) app(extra ...string) outcome {
	h.t.Helper()
	return h.run(h.appArgs(extra...)...)
}

func (h *harness) dnsSync(extra ...string) outcome {
	h.t.Helper()
	return h.run(append([]string{"dns-sync", "--infra", h.infra, "--rev", h.rev,
		"--porkbun-api-key-file", filepath.Join(h.root, "creds", "porkbun-key"),
		"--porkbun-secret-key-file", filepath.Join(h.root, "creds", "porkbun-secret")}, extra...)...)
}

func (h *harness) calls() []call {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.root, "calls.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	var calls []call
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var c call
		if err := json.Unmarshal(line, &c); err != nil {
			h.t.Fatal(err)
		}
		calls = append(calls, c)
	}
	return calls
}

// toolCalls is callNames without git: the order of nix, cue, dns, flyctl and
// skopeo calls is the contract, the source phase's git steps are not.
func (h *harness) toolCalls() []string {
	var out []string
	for _, name := range h.callNames() {
		if !strings.HasPrefix(name, "git ") {
			out = append(out, name)
		}
	}
	return out
}

// callNames lists the calls as "tool subcommand", e.g. "flyctl deploy".
func (h *harness) callNames() []string {
	var names []string
	for _, c := range h.calls() {
		name := c.Tool
		args := c.Args
		if c.Tool == "skopeo" && len(args) > 6 {
			args = args[6:]
		}
		for len(args) > 2 && args[0] == "-c" {
			args = args[2:]
		}
		if len(args) > 0 {
			name += " " + args[0]
		}
		names = append(names, name)
	}
	return names
}

// liveWorld reads the fake world as the fakes left it.
func (h *harness) liveWorld() *world {
	h.t.Helper()
	var w world
	if err := readJSON(filepath.Join(h.root, "world.json"), &w); err != nil {
		h.t.Fatal(err)
	}
	return &w
}

// phases renders the record's phases as "name outcome".
func (o outcome) phases() []string {
	var out []string
	if o.rec != nil {
		for _, p := range o.rec.Phases {
			out = append(out, p.Name+" "+p.Outcome)
		}
	}
	return out
}

func (o outcome) reason(phase string) string {
	if o.rec != nil {
		for _, p := range o.rec.Phases {
			if p.Name == phase && p.Reason != nil {
				return *p.Reason
			}
		}
	}
	return ""
}

func wantRun(t *testing.T, o outcome, code int, outcomeWant string, phases ...string) {
	t.Helper()
	if o.code != code {
		t.Errorf("exit status %d, want %d\nstdout:\n%s\nstderr:\n%s", o.code, code, o.stdout, o.stderr)
	}
	if o.rec == nil {
		t.Fatalf("no record\nstdout:\n%s\nstderr:\n%s", o.stdout, o.stderr)
	}
	if o.rec.Outcome != outcomeWant {
		t.Errorf("outcome %q, want %q\nstderr:\n%s", o.rec.Outcome, outcomeWant, o.stderr)
	}
	if got := o.phases(); !slices.Equal(got, phases) {
		t.Errorf("phases %q, want %q\nstderr:\n%s", got, phases, o.stderr)
	}
}

// wantCleanedUp checks the attempt left only its private record behind.
func wantCleanedUp(t *testing.T, o outcome) {
	t.Helper()
	runDir := filepath.Dir(o.recordPath)
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"logs", "record.json"}) {
		t.Errorf("run directory holds %v, want only logs and record.json", names)
	}
	logs, _ := filepath.Glob(filepath.Join(runDir, "logs", "*.log"))
	if len(logs) == 0 {
		t.Error("no child logs kept")
	}
	for _, log := range logs {
		if st, err := os.Stat(log); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v (%v), want 0600", log, st.Mode().Perm(), err)
		}
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(runDir, "logs"): 0o700,
		runDir:                        0o700,
		filepath.Dir(runDir):          0o700,
		o.recordPath:                  0o600,
		filepath.Join(filepath.Dir(filepath.Dir(runDir)), "deploy.lock"): 0o600,
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Error(err)
		} else if st.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", path, st.Mode().Perm(), want)
		}
	}
}

func phaseNames(outcomes ...string) []string {
	names := []string{"source", "prepare", "inspect", "upload", "apply", "verify", "cleanup"}
	var out []string
	for i, o := range outcomes {
		out = append(out, names[i]+" "+o)
	}
	return out
}

// readAll concatenates the files.
func readAll(t *testing.T, paths ...string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}

// namedLog returns the log path deploy printed for the child whose log name
// contains name.
func namedLog(t *testing.T, stderr, name string) string {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		if _, path, ok := strings.Cut(line, "; log: "); ok && strings.Contains(filepath.Base(path), name) {
			return path
		}
	}
	t.Fatalf("deploy named no log for %s:\n%s", name, stderr)
	return ""
}
