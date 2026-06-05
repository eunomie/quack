# Multi-user sandbox hardening — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the owner invite trusted-friend Discord users who can only run agents inside per-session Docker sandboxes (agent container + `docker:dind` sidecar), while the owner keeps today's full host access.

**Architecture:** A per-session `Launcher` seam in `internal/agentproc` decides *where* a turn's child process runs — on the host (`DirectLauncher`, today's behavior, owner path) or inside a guest's container (`ContainerLauncher` → `docker exec`). A new `internal/sandbox` package provisions/tears down the container set behind a `session.Sandboxer` interface. `internal/session` gates workspace prep, headless, and tool policy on a per-request `Role`; `internal/discord` resolves owner-vs-guest from Discord ids/roles. Owner sessions create no containers and are byte-for-byte unchanged.

**Tech Stack:** Go 1.x, the `docker` CLI (shelled out, like `gitexec`/`tmuxexec`), `docker:dind`, a tiny Go egress proxy, discordgo, BurntSushi/toml.

**Reference:** design doc `hack/designs/2026-06-05-multi-user-sandbox-hardening.md`. Read it first.

**Conventions (this repo):** Version control is **Stacked Git** — each task ends by creating/refreshing an stg patch (`stg new <name> -m "…"` then `stg refresh`), every patch carries `Signed-off-by: Yves Brissaud <yves.brissaud@gmail.com>` and **no** AI `Co-Authored-By`. Tests: `go test ./...`; vet: `go vet ./...`. Integration tests gate on `QUACK_INTEGRATION=1` and shell out to real tools. Keep `session` depending on its own interfaces; adapters may import `session` for shared types (as `tmuxexec` does).

---

## Prerequisites & spikes (do first — resolve the design's flagged unknowns)

These are short investigations whose results feed later tasks. Each ends by recording the answer in a comment block at the top of `hack/sandbox/NOTES.md` (create it) and committing.

### Task P1: Confirm minimal model-credential files

**Files:** Create `hack/sandbox/NOTES.md`.

- [ ] **Step 1:** On the host, identify the smallest file(s) that authenticate each CLI without exposing other data:
  - claude: inspect `~/.claude/` and `~/.claude.json`. Find the OAuth/credential file (e.g. `~/.claude/.credentials.json`). Confirm `claude -p hi --output-format json` works in a clean `$HOME` that contains only that file (plus an empty `~/.claude.json` if required). Run:
    ```bash
    tmpd=$(mktemp -d); cp ~/.claude/.credentials.json "$tmpd/" 2>/dev/null; \
    HOME="$tmpd" claude -p 'say ok' --output-format json --permission-mode plan; echo "exit=$?"
    ```
  - codex: same for `~/.codex/auth.json`:
    ```bash
    tmpd=$(mktemp -d); mkdir -p "$tmpd/.codex"; cp ~/.codex/auth.json "$tmpd/.codex/"; \
    HOME="$tmpd" codex exec --json 'say ok'; echo "exit=$?"
    ```
- [ ] **Step 2:** Record the exact minimal path list per agent in `NOTES.md` (these become the `model_cred_mounts` defaults in Task 13).
- [ ] **Step 3:** Commit.
  ```bash
  stg new sandbox-notes-creds -m "$(printf 'sandbox: record minimal model-credential paths\n\nSigned-off-by: Yves Brissaud <yves.brissaud@gmail.com>')"
  stg refresh
  ```

### Task P2: Confirm egress host list per agent

- [ ] **Step 1:** With a host-level deny-all-but-allow proxy (or by reading vendor docs), determine the domains each CLI needs: Anthropic (`api.anthropic.com`, plus any telemetry/console hosts claude refuses to start without), OpenAI/codex auth + inference hosts, and GitHub (`github.com`, `api.github.com`, `codeload.github.com`). Validate by running each CLI through the Task-4 proxy with only those hosts allowed.
- [ ] **Step 2:** Record the validated allow-list in `NOTES.md` (becomes the `egress_allow` default in Task 12). Commit as `stg new sandbox-notes-egress`.

### Task P3: Confirm claude per-skill allow/deny matcher

- [ ] **Step 1:** Determine how `claude` filters an individual skill. Test candidates against a build that has the `revue` and `zed`/`open-zed` skills:
  ```bash
  claude -p 'list available skills' --disallowed-tools 'Skill(open-zed)' --output-format json
  ```
  Try matcher forms (`Skill(open-zed)`, `mcp__…`, the plugin-qualified name) until one provably hides `open-zed` while leaving `revue` usable. If no per-skill matcher exists, fall back to an allow-list of the non-skill tools plus the permitted skill, or omit the `Skill` tool entirely for guests.
- [ ] **Step 2:** Record the working matcher + the chosen guest default (`disallowed_skills=["open-zed"]`, `allowed_skills=["revue"]`) and how it maps to claude flags, in `NOTES.md`. Commit as `stg new sandbox-notes-skills`.

### Task P4: Confirm dind sibling + TLS wiring works on the host

- [ ] **Step 1:** Manually stand up the pair to prove the pattern before coding it:
  ```bash
  docker network create q-int-test --internal
  docker network create q-ext-test
  docker volume create q-certs-test
  docker run -d --name q-dind-test --privileged --network q-ext-test \
    -e DOCKER_TLS_CERTDIR=/certs -v q-certs-test:/certs docker:dind
  docker network connect q-int-test q-dind-test
  docker run -d --name q-agent-test --network q-int-test \
    -e DOCKER_HOST=tcp://q-dind-test:2376 -e DOCKER_TLS_VERIFY=1 -e DOCKER_CERT_PATH=/certs/client \
    -v q-certs-test:/certs:ro --entrypoint sleep docker:cli infinity
  # wait for dind to generate certs, then:
  docker exec q-agent-test docker info   # MUST succeed and show the INNER daemon
  docker exec q-agent-test docker run --rm hello-world   # inner daemon pulls+runs
  ```
- [ ] **Step 2:** Confirm the agent on the `--internal` network cannot reach the internet directly (`docker exec q-agent-test wget -T3 -qO- https://example.com` should hang/fail), but the dind sidecar can pull images. Record the exact working command sequence + any timing/wait needed for certs in `NOTES.md`.
- [ ] **Step 3:** Tear down (`docker rm -f q-dind-test q-agent-test; docker network rm q-int-test q-ext-test; docker volume rm q-certs-test`). Commit notes as `stg new sandbox-notes-dind`.

---

## Phase 1 — The Launcher seam (`internal/agentproc`)

Goal: introduce the launcher abstraction and route both drivers through it, with **zero behavior change** (default `DirectLauncher` reproduces today exactly).

### Task 1: Define the Launcher interface and DirectLauncher

**Files:**
- Create: `internal/agentproc/launcher.go`
- Test: `internal/agentproc/launcher_test.go`

- [ ] **Step 1: Write the failing test**
```go
package agentproc

import (
	"context"
	"testing"
)

func TestDirectLauncherSetsProgramArgsDir(t *testing.T) {
	cmd := DirectLauncher{}.Command(context.Background(), "claude", []string{"-p", "hi"}, "/work", nil)
	if cmd.Args[0] != "claude" || cmd.Args[1] != "-p" || cmd.Args[2] != "hi" {
		t.Fatalf("argv = %v", cmd.Args)
	}
	if cmd.Dir != "/work" {
		t.Fatalf("dir = %q", cmd.Dir)
	}
}
```
- [ ] **Step 2: Run to verify it fails** — `go test ./internal/agentproc/ -run TestDirectLauncher` → FAIL (undefined `DirectLauncher`).
- [ ] **Step 3: Implement**
```go
package agentproc

import (
	"context"
	"os"
	"os/exec"
)

// Launcher turns a command (program + args, working dir, extra env) into the
// *exec.Cmd the driver runs. DirectLauncher runs it on the host — quack's
// original behavior. A sandbox launcher (ContainerLauncher) wraps it so a guest
// turn runs inside a container. The driver builds the same argv either way; the
// launcher only decides where it executes.
type Launcher interface {
	Command(ctx context.Context, program string, args []string, dir string, env []string) *exec.Cmd
}

// DirectLauncher runs the command directly on the host.
type DirectLauncher struct{}

func (DirectLauncher) Command(ctx context.Context, program string, args []string, dir string, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd
}
```
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/agentproc/ -run TestDirectLauncher` → PASS.
- [ ] **Step 5: Commit** — `stg new agentproc-launcher-iface` + refresh (sign-off trailer).

### Task 2: Add ContainerLauncher (docker exec wrapper)

**Files:**
- Modify: `internal/agentproc/launcher.go`
- Test: `internal/agentproc/launcher_test.go`

- [ ] **Step 1: Write the failing test**
```go
func TestContainerLauncherWrapsDockerExec(t *testing.T) {
	l := ContainerLauncher{Container: "q-agent", Workdir: "/work/repo", DockerCmd: "docker"}
	cmd := l.Command(context.Background(), "claude", []string{"-p", "hi"}, "/ignored/host/path", []string{"FOO=bar"})
	want := []string{"docker", "exec", "-i", "-w", "/work/repo", "-e", "FOO=bar", "q-agent", "claude", "-p", "hi"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("argv = %v", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full %v)", i, cmd.Args[i], want[i], cmd.Args)
		}
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL (undefined `ContainerLauncher`).
- [ ] **Step 3: Implement** (append to `launcher.go`)
```go
// ContainerLauncher runs the command inside an already-running container via
// `docker exec`. Workdir is the in-container path (the host dir passed by the
// driver is ignored — the clone is mounted at a fixed path inside the box).
type ContainerLauncher struct {
	Container string
	Workdir   string
	DockerCmd string // defaults to "docker"; injectable for tests
}

func (c ContainerLauncher) Command(ctx context.Context, program string, args []string, dir string, env []string) *exec.Cmd {
	docker := c.DockerCmd
	if docker == "" {
		docker = "docker"
	}
	full := []string{"exec", "-i", "-w", c.Workdir}
	for _, e := range env {
		full = append(full, "-e", e)
	}
	full = append(full, c.Container, program)
	full = append(full, args...)
	return exec.CommandContext(ctx, docker, full...)
}
```
- [ ] **Step 4: Run to verify it passes** — PASS.
- [ ] **Step 5: Commit** — `stg new agentproc-container-launcher` + refresh.

### Task 3: Route both drivers through the launcher

**Files:**
- Modify: `internal/agentproc/driver.go` (add `Launcher` field to `Turn`)
- Modify: `internal/agentproc/claude.go:53-59` (`RunTurn`)
- Modify: `internal/agentproc/codex.go:30-36` (`RunTurn`)
- Test: `internal/agentproc/launcher_test.go`

- [ ] **Step 1: Write the failing test** — prove a turn runs through an injected launcher by faking it with a script that emits a canned claude stream.
```go
type fakeLauncher struct {
	gotProgram string
	gotArgs    []string
	gotDir     string
}

func (f *fakeLauncher) Command(ctx context.Context, program string, args []string, dir string, env []string) *exec.Cmd {
	f.gotProgram, f.gotArgs, f.gotDir = program, args, dir
	// Emit one assistant-text line and a result line, mimicking claude stream-json.
	const stream = `{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"hello"}]}}
{"type":"result","session_id":"s1","is_error":false,"total_cost_usd":0}`
	return exec.CommandContext(ctx, "printf", "%s", stream)
}

func TestClaudeRunTurnUsesLauncher(t *testing.T) {
	f := &fakeLauncher{}
	var texts []string
	done := Claude{}.RunTurn(context.Background(), Turn{Prompt: "hi", Workdir: "/work", Launcher: f}, func(e Event) {
		if a, ok := e.(AssistantText); ok {
			texts = append(texts, a.Text)
		}
	})
	if done.Err != nil {
		t.Fatalf("err: %v", done.Err)
	}
	if f.gotProgram != "claude" || f.gotDir != "/work" {
		t.Fatalf("program=%q dir=%q", f.gotProgram, f.gotDir)
	}
	if done.SessionRef != "s1" || len(texts) != 1 || texts[0] != "hello" {
		t.Fatalf("ref=%q texts=%v", done.SessionRef, texts)
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL (no `Launcher` field on `Turn`).
- [ ] **Step 3: Implement** — add the field to `Turn` in `driver.go`:
```go
// Launcher decides where the turn's child process runs (host vs container).
// nil means run directly on the host (DirectLauncher).
Launcher Launcher
```
  Then in `claude.go` `RunTurn`, replace the `exec.CommandContext`/`cmd.Dir` lines (currently lines 58-59) with:
```go
	l := t.Launcher
	if l == nil {
		l = DirectLauncher{}
	}
	cmd := l.Command(ctx, command, d.args(t), t.Workdir, nil)
```
  Apply the identical change in `codex.go` `RunTurn` (replace its lines 35-36), using `d.args(t)` for codex.
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/agentproc/...` → PASS. Confirm no other call site breaks: `go build ./...`.
- [ ] **Step 5: Commit** — `stg new agentproc-route-launcher` + refresh.

> After Phase 1: owner sessions still pass `Launcher: nil` (set nowhere yet) → DirectLauncher → identical to today. Verify with `go test ./...` and a manual owner session smoke test if convenient.

---

## Phase 2 — The sandbox package (`internal/sandbox`)

Goal: a Docker-backed provisioner that stands up and tears down a guest's container set, unit-tested against a fake command runner, with the real path behind `QUACK_INTEGRATION`. Not wired to sessions yet.

### Task 4: The egress allow-list proxy image

**Files:**
- Create: `hack/sandbox/proxy/main.go`
- Create: `hack/sandbox/proxy/Dockerfile`
- Test: `hack/sandbox/proxy/main_test.go`

- [ ] **Step 1: Write the failing test** for the host-matching logic.
```go
package main

import "testing"

func TestAllowedHost(t *testing.T) {
	allow := parseAllow("api.anthropic.com,github.com,api.github.com")
	cases := map[string]bool{
		"api.anthropic.com:443":  true,
		"github.com:443":         true,
		"codeload.github.com:443": false, // not listed
		"evil.com:443":           false,
		"api.github.com:443":     true,
	}
	for hostport, want := range cases {
		if got := allowed(allow, hostport); got != want {
			t.Fatalf("allowed(%q) = %v, want %v", hostport, got, want)
		}
	}
}
```
- [ ] **Step 2: Run to verify it fails** — `go test ./hack/sandbox/proxy/` → FAIL.
- [ ] **Step 3: Implement** a minimal HTTPS-CONNECT allow-list proxy.
```go
// Command proxy is a minimal forward proxy that only tunnels CONNECT requests to
// an explicit allow-list of hosts. Guest agent containers point HTTPS_PROXY at
// it; everything not on the list is refused, bounding egress.
package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func parseAllow(s string) map[string]bool {
	m := map[string]bool{}
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			m[strings.ToLower(h)] = true
		}
	}
	return m
}

func allowed(allow map[string]bool, hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return allow[strings.ToLower(host)]
}

func main() {
	allow := parseAllow(os.Getenv("ALLOW"))
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8888"
	}
	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect || !allowed(allow, r.Host) {
				http.Error(w, "forbidden", http.StatusForbidden)
				log.Printf("DENY %s %s", r.Method, r.Host)
				return
			}
			dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
			if err != nil {
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
			hj, ok := w.(http.Hijacker)
			if !ok {
				dst.Close()
				return
			}
			src, _, err := hj.Hijack()
			if err != nil {
				dst.Close()
				return
			}
			go func() { io.Copy(dst, src); dst.Close() }()
			io.Copy(src, dst)
			src.Close()
		}),
	}
	log.Printf("egress proxy on %s, allow=%v", addr, allow)
	log.Fatal(srv.ListenAndServe())
}
```
```dockerfile
# hack/sandbox/proxy/Dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY main.go .
RUN go build -o /proxy main.go
FROM alpine:3.20
COPY --from=build /proxy /proxy
EXPOSE 8888
ENTRYPOINT ["/proxy"]
```
- [ ] **Step 4: Run to verify it passes** — `go test ./hack/sandbox/proxy/` → PASS.
- [ ] **Step 5: Commit** — `stg new sandbox-egress-proxy` + refresh.

> Note: this proxy intentionally only handles HTTPS CONNECT (all model/git traffic is TLS). Plain HTTP is refused, which is fine and desirable.

### Task 5: The sandbox runtime image

**Files:** Create `hack/sandbox/Dockerfile`, `hack/sandbox/README.md`.

- [ ] **Step 1:** Author the guest agent image: a base with `git`, `gh`, the Docker **CLI** (not daemon), Node (for claude), the `claude` and `codex` CLIs, and common build tools. Pin versions where practical.
```dockerfile
# hack/sandbox/Dockerfile — the guest agent container image.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git curl gnupg docker.io openssh-client build-essential && \
    rm -rf /var/lib/apt/lists/*
# gh
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg && \
    echo "deb [signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list && \
    apt-get update && apt-get install -y gh && rm -rf /var/lib/apt/lists/*
# node + agent CLIs (adjust install commands to the real distribution channels)
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && \
    apt-get install -y nodejs && npm i -g @anthropic-ai/claude-code @openai/codex && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /work
```
  (Exact CLI install commands per the vendors' current channels — verify in P1.)
- [ ] **Step 2:** Write `hack/sandbox/README.md`: how to build both images (`docker build -t quack-sandbox:latest hack/sandbox`, `docker build -t quack-egress:latest hack/sandbox/proxy`) and that they are a setup prerequisite.
- [ ] **Step 3:** Build both locally to confirm they build (`docker build …`). Record image names in `NOTES.md`.
- [ ] **Step 4: Commit** — `stg new sandbox-image` + refresh.

### Task 6: Docker CLI wrapper

**Files:**
- Create: `internal/sandbox/docker.go`
- Test: `internal/sandbox/docker_test.go`

- [ ] **Step 1: Write the failing test** using an injected runner that records argv.
```go
package sandbox

import (
	"context"
	"reflect"
	"testing"
)

func TestDockerCommandsBuildExpectedArgv(t *testing.T) {
	var calls [][]string
	d := &Docker{run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}}
	ctx := context.Background()
	_ = d.CreateNetwork(ctx, "q-int", true)
	_ = d.CreateVolume(ctx, "q-work")
	_ = d.Remove(ctx, "q-agent")
	want := [][]string{
		{"docker", "network", "create", "--internal", "q-int"},
		{"docker", "volume", "create", "q-work"},
		{"docker", "rm", "-f", "q-agent"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v", calls)
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement** `docker.go`.
```go
// Package sandbox provisions and tears down the per-guest-session Docker
// containers (agent + dind sidecar + egress proxy) that confine a guest agent.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %v: %w: %s", name, args, err, out.String())
	}
	return out.Bytes(), nil
}

// Docker is a thin wrapper over the docker CLI (shelled out, like gitexec).
type Docker struct{ run runner }

func NewDocker() *Docker { return &Docker{run: execRun} }

func (d *Docker) CreateNetwork(ctx context.Context, name string, internal bool) error {
	args := []string{"network", "create"}
	if internal {
		args = append(args, "--internal")
	}
	args = append(args, name)
	_, err := d.run(ctx, "docker", args...)
	return err
}

func (d *Docker) ConnectNetwork(ctx context.Context, network, container string) error {
	_, err := d.run(ctx, "docker", "network", "connect", network, container)
	return err
}

func (d *Docker) RemoveNetwork(ctx context.Context, name string) error {
	_, err := d.run(ctx, "docker", "network", "rm", name)
	return err
}

func (d *Docker) CreateVolume(ctx context.Context, name string) error {
	_, err := d.run(ctx, "docker", "volume", "create", name)
	return err
}

func (d *Docker) RemoveVolume(ctx context.Context, name string) error {
	_, err := d.run(ctx, "docker", "volume", "rm", "-f", name)
	return err
}

// Run starts a detached container; args are the full `docker run` arguments
// after `run -d`. Returns the container id/name line.
func (d *Docker) Run(ctx context.Context, args ...string) error {
	_, err := d.run(ctx, "docker", append([]string{"run", "-d"}, args...)...)
	return err
}

func (d *Docker) Exec(ctx context.Context, container string, argv ...string) ([]byte, error) {
	return d.run(ctx, "docker", append([]string{"exec", container}, argv...)...)
}

func (d *Docker) Remove(ctx context.Context, container string) error {
	_, err := d.run(ctx, "docker", "rm", "-f", container)
	return err
}

func (d *Docker) Exists(ctx context.Context, container string) bool {
	_, err := d.run(ctx, "docker", "inspect", "--type", "container", container)
	return err == nil
}
```
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/sandbox/` → PASS.
- [ ] **Step 5: Commit** — `stg new sandbox-docker-wrapper` + refresh.

### Task 7: Provisioner — types + provision sequence

**Files:**
- Create: `internal/sandbox/sandbox.go`
- Test: `internal/sandbox/sandbox_test.go`

- [ ] **Step 1: Write the failing test** asserting the provision sequence creates the network/volume/proxy/dind/agent in order and clones when a repo URL is given. Use the fake runner; match on key argv fragments.
```go
package sandbox

import (
	"context"
	"strings"
	"testing"
)

func recordingDocker() (*Docker, *[][]string) {
	var calls [][]string
	d := &Docker{run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("ok"), nil
	}}
	return d, &calls
}

func hasCall(calls [][]string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

func TestProvisionCreatesContainerSetAndClones(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "quack-sandbox:latest", ProxyImage: "quack-egress:latest", DindImage: "docker:dind"}
	sb, err := p.Provision(context.Background(), Spec{
		SessionName: "feat-x",
		RepoURL:     "https://github.com/o/r",
		CloneRef:    "main",
		GitHubPAT:   "PAT",
		GitUserName: "Owner", GitUserEmail: "o@e",
		EgressAllow: []string{"github.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.AgentContainer == "" || sb.Workdir == "" {
		t.Fatalf("handle incomplete: %+v", sb)
	}
	for _, want := range []string{
		"network create --internal", "volume create",
		"quack-egress:latest", "docker:dind", "quack-sandbox:latest",
		"git clone", // repo cloned into the volume
	} {
		if !hasCall(*calls, want) {
			t.Fatalf("missing call containing %q in %v", want, *calls)
		}
	}
}

func TestProvisionEmptySandboxSkipsClone(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d, AgentImage: "i", ProxyImage: "px", DindImage: "docker:dind"}
	sb, err := p.Provision(context.Background(), Spec{SessionName: "q", EgressAllow: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if hasCall(*calls, "git clone") {
		t.Fatal("empty sandbox should not clone")
	}
	if sb.Workdir != "/work" {
		t.Fatalf("workdir = %q, want /work", sb.Workdir)
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement** `sandbox.go`. Names are derived from a sanitized session name; the model-cred mounts and env are passed through to `docker run` of the agent container.
```go
package sandbox

import (
	"context"
	"fmt"
	"strings"
)

// Mount is a host:container bind, mounted read-only into the agent container
// (used for the minimal model-credential files).
type Mount struct {
	Host      string
	Container string
}

// Spec describes one guest session's sandbox.
type Spec struct {
	SessionName  string
	RepoURL      string // "" => empty sandbox (no clone)
	CloneRef     string // base branch/ref to clone
	RepoDir      string // in-container dir name for the clone; default basename of RepoURL
	GitHubPAT    string
	GitUserName  string
	GitUserEmail string
	ModelMounts  []Mount
	AgentEnv     []string // extra env for the agent container (e.g. GH_TOKEN passed via store instead)
	EgressAllow  []string
}

// Handle identifies a provisioned sandbox. Persisted in the session record so a
// restart can reattach or rebuild it.
type Handle struct {
	Name           string `json:"name"`
	AgentContainer string `json:"agent_container"`
	DindContainer  string `json:"dind_container"`
	ProxyContainer string `json:"proxy_container"`
	IntNetwork     string `json:"int_network"`
	ExtNetwork     string `json:"ext_network"`
	CertVolume     string `json:"cert_volume"`
	WorkVolume     string `json:"work_volume"`
	Workdir        string `json:"workdir"` // in-container cwd
}

type DockerProvisioner struct {
	D          *Docker
	AgentImage string
	ProxyImage string
	DindImage  string
	ProxyPort  string // default "8888"
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return '-'
	}, s)
}

func (p *DockerProvisioner) Provision(ctx context.Context, spec Spec) (*Handle, error) {
	port := p.ProxyPort
	if port == "" {
		port = "8888"
	}
	n := sanitize(spec.SessionName)
	h := &Handle{
		Name:           n,
		AgentContainer: "quack-" + n + "-agent",
		DindContainer:  "quack-" + n + "-dind",
		ProxyContainer: "quack-" + n + "-proxy",
		IntNetwork:     "quack-" + n + "-int",
		ExtNetwork:     "quack-" + n + "-ext",
		CertVolume:     "quack-" + n + "-certs",
		WorkVolume:     "quack-" + n + "-work",
		Workdir:        "/work",
	}

	// Networks + volumes.
	if err := p.D.CreateNetwork(ctx, h.IntNetwork, true); err != nil {
		return nil, err
	}
	if err := p.D.CreateNetwork(ctx, h.ExtNetwork, false); err != nil {
		return nil, err
	}
	if err := p.D.CreateVolume(ctx, h.CertVolume); err != nil {
		return nil, err
	}
	if err := p.D.CreateVolume(ctx, h.WorkVolume); err != nil {
		return nil, err
	}

	// Egress proxy: on the internal net (agent reaches it) + external net (it reaches the allow-list).
	if err := p.D.Run(ctx, "--name", h.ProxyContainer, "--network", h.IntNetwork,
		"-e", "ALLOW="+strings.Join(spec.EgressAllow, ","), "-e", "ADDR=:"+port, p.ProxyImage); err != nil {
		return nil, err
	}
	if err := p.D.ConnectNetwork(ctx, h.ExtNetwork, h.ProxyContainer); err != nil {
		return nil, err
	}

	// dind sidecar: external net for registry pulls, also internal so the agent reaches its API.
	if err := p.D.Run(ctx, "--name", h.DindContainer, "--privileged", "--network", h.ExtNetwork,
		"-e", "DOCKER_TLS_CERTDIR=/certs", "-v", h.CertVolume+":/certs", p.DindImage); err != nil {
		return nil, err
	}
	if err := p.D.ConnectNetwork(ctx, h.IntNetwork, h.DindContainer); err != nil {
		return nil, err
	}

	// Agent container: internal net only (no direct egress), proxy + dind reachable.
	runArgs := []string{"--name", h.AgentContainer, "--network", h.IntNetwork,
		"-v", h.WorkVolume + ":/work",
		"-v", h.CertVolume + ":/certs:ro",
		"-e", "HTTPS_PROXY=http://" + h.ProxyContainer + ":" + port,
		"-e", "HTTP_PROXY=http://" + h.ProxyContainer + ":" + port,
		"-e", "NO_PROXY=" + h.DindContainer + ",localhost,127.0.0.1",
		"-e", "DOCKER_HOST=tcp://" + h.DindContainer + ":2376",
		"-e", "DOCKER_TLS_VERIFY=1",
		"-e", "DOCKER_CERT_PATH=/certs/client",
		"-e", "GIT_AUTHOR_NAME=" + spec.GitUserName,
		"-e", "GIT_COMMITTER_NAME=" + spec.GitUserName,
		"-e", "GIT_AUTHOR_EMAIL=" + spec.GitUserEmail,
		"-e", "GIT_COMMITTER_EMAIL=" + spec.GitUserEmail,
		"-e", "GH_TOKEN=" + spec.GitHubPAT,
	}
	for _, e := range spec.AgentEnv {
		runArgs = append(runArgs, "-e", e)
	}
	for _, m := range spec.ModelMounts {
		runArgs = append(runArgs, "-v", m.Host+":"+m.Container+":ro")
	}
	runArgs = append(runArgs, "--entrypoint", "sleep", p.AgentImage, "infinity")
	if err := p.D.Run(ctx, runArgs...); err != nil {
		return nil, err
	}

	// Seed git credentials (HTTPS store) so push/gh work without an SSH key.
	cred := fmt.Sprintf("https://x-access-token:%s@github.com", spec.GitHubPAT)
	seed := "git config --global credential.helper store && " +
		"git config --global user.name \"$GIT_AUTHOR_NAME\" && " +
		"git config --global user.email \"$GIT_AUTHOR_EMAIL\" && " +
		"printf '%s\\n' '" + cred + "' > ~/.git-credentials && chmod 600 ~/.git-credentials"
	if _, err := p.D.Exec(ctx, h.AgentContainer, "sh", "-lc", seed); err != nil {
		return nil, err
	}

	// Clone the repo into the work volume (empty sandbox if no RepoURL).
	if spec.RepoURL != "" {
		dir := spec.RepoDir
		if dir == "" {
			dir = repoBase(spec.RepoURL)
		}
		clone := []string{"git", "clone"}
		if spec.CloneRef != "" {
			clone = append(clone, "--branch", spec.CloneRef)
		}
		clone = append(clone, spec.RepoURL, "/work/"+dir)
		if _, err := p.D.Exec(ctx, h.AgentContainer, clone...); err != nil {
			return nil, err
		}
		h.Workdir = "/work/" + dir
	}
	return h, nil
}

func repoBase(url string) string {
	u := strings.TrimSuffix(url, ".git")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}
```
- [ ] **Step 4: Run to verify it passes** — PASS.
- [ ] **Step 5: Commit** — `stg new sandbox-provision` + refresh.

### Task 8: Teardown, Reattach, Launcher

**Files:**
- Modify: `internal/sandbox/sandbox.go`
- Test: `internal/sandbox/sandbox_test.go`

- [ ] **Step 1: Write the failing test**
```go
func TestTeardownRemovesEverything(t *testing.T) {
	d, calls := recordingDocker()
	p := &DockerProvisioner{D: d}
	h := &Handle{AgentContainer: "a", DindContainer: "dd", ProxyContainer: "px",
		IntNetwork: "int", ExtNetwork: "ext", CertVolume: "cv", WorkVolume: "wv"}
	if err := p.Teardown(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rm -f a", "rm -f dd", "rm -f px", "network rm int", "network rm ext", "volume rm -f wv"} {
		if !hasCall(*calls, want) {
			t.Fatalf("teardown missing %q in %v", want, *calls)
		}
	}
}

func TestLauncherBindsAgentContainer(t *testing.T) {
	p := &DockerProvisioner{}
	l := p.Launcher(&Handle{AgentContainer: "q-agent", Workdir: "/work/r"})
	cmd := l.Command(context.Background(), "claude", []string{"-p", "x"}, "/host", nil)
	got := strings.Join(cmd.Args, " ")
	if !strings.Contains(got, "exec -i -w /work/r q-agent claude -p x") {
		t.Fatalf("launcher argv = %q", got)
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement** (append to `sandbox.go`); import `agentproc`.
```go
import "github.com/eunomie/quack/internal/agentproc"

// Teardown removes the whole container set. Best-effort: it continues past
// individual failures so a partial provision can still be cleaned up.
func (p *DockerProvisioner) Teardown(ctx context.Context, h *Handle) error {
	for _, c := range []string{h.AgentContainer, h.DindContainer, h.ProxyContainer} {
		if c != "" {
			_ = p.D.Remove(ctx, c)
		}
	}
	for _, n := range []string{h.IntNetwork, h.ExtNetwork} {
		if n != "" {
			_ = p.D.RemoveNetwork(ctx, n)
		}
	}
	for _, v := range []string{h.CertVolume, h.WorkVolume} {
		if v != "" {
			_ = p.D.RemoveVolume(ctx, v)
		}
	}
	return nil
}

// Launcher returns the per-session launcher that runs turns inside the agent
// container.
func (p *DockerProvisioner) Launcher(h *Handle) agentproc.Launcher {
	return agentproc.ContainerLauncher{Container: h.AgentContainer, Workdir: h.Workdir}
}
```
- [ ] **Step 4:** Add `Reattach` — if the agent container is gone but the work volume persists (host reboot), recreate the container set bound to the existing volumes; if it already exists, no-op. (Implement by checking `p.D.Exists(ctx, h.AgentContainer)`; if false, re-run the network/proxy/dind/agent `docker run` steps from `Provision` **without** re-cloning, since the work volume already holds the clone. Extract the container-creation steps of `Provision` into a private `bringUp(ctx, h, spec)` helper that both `Provision` and `Reattach` call; `Reattach` reconstructs the minimal spec it needs — egress/PAT/mounts — from fields stored on the handle, so persist those on `Handle` too: add `EgressAllow []string`, `GitHubPAT string` (or re-source the PAT from config at rehydrate, preferred — see Task 17).) Write a test `TestReattachRecreatesWhenAgentMissing` using a fake runner whose `Exists` returns false.
- [ ] **Step 5: Commit** — `stg new sandbox-teardown-launcher` + refresh.

### Task 9: Integration test (real Docker, gated)

**Files:** Create `internal/sandbox/sandbox_integration_test.go`.

- [ ] **Step 1:** Write a test guarded by `QUACK_INTEGRATION=1` that, using `NewDocker()` and the real images from Task 5/4, provisions an **empty** sandbox, execs `docker info` inside the agent (proves dind sibling works), execs `wget`/`curl` to a disallowed host and asserts it fails (proves egress), then tears everything down. Skip if `QUACK_INTEGRATION` unset.
```go
//go:build integration_docker
// (or runtime check: if os.Getenv("QUACK_INTEGRATION") == "" { t.Skip(...) })
```
- [ ] **Step 2:** Run `QUACK_INTEGRATION=1 go test ./internal/sandbox/ -run Integration -v`; confirm PASS and clean teardown (`docker ps -a | grep quack-` is empty after).
- [ ] **Step 3: Commit** — `stg new sandbox-integration` + refresh.

---

## Phase 3 — Config & guest policy (`internal/config`, `internal/agent`)

### Task 10: Owner & guest-role config fields

**Files:**
- Modify: `internal/config/config.go` (`Discord` struct + accessors)
- Test: `internal/config/config_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**
```go
func TestOwnerAndGuestAccessors(t *testing.T) {
	d := Discord{
		AllowedUserID:  "legacy",
		OwnerUserIDs:   []string{"owner1"},
		GuestRoleID:    "grole",
		GuestRoleIDs:   []string{"grole2"},
	}
	owners := d.OwnerIDs()
	if len(owners) != 2 || owners[0] != "owner1" || owners[1] != "legacy" {
		t.Fatalf("owners = %v", owners)
	}
	roles := d.GuestRoles()
	if len(roles) != 2 || roles[0] != "grole" || roles[1] != "grole2" {
		t.Fatalf("guest roles = %v", roles)
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement** — add to `Discord` struct: `OwnerUserID string`, `OwnerUserIDs []string`, `GuestRoleID string`, `GuestRoleIDs []string` (with toml tags `owner_user_id`, `owner_user_ids`, `guest_role_id`, `guest_role_ids`). Add accessors:
```go
// OwnerIDs are full-access users: the explicit owner_user_id(s) plus the legacy
// allowed_user_id(s), so an existing single-user config keeps full access.
func (d Discord) OwnerIDs() []string {
	return append(mergeIDs(d.OwnerUserID, d.OwnerUserIDs), mergeIDs(d.AllowedUserID, d.AllowedUserIDs)...)
}

// GuestRoles are the Discord role ids whose members get the sandbox.
func (d Discord) GuestRoles() []string { return mergeIDs(d.GuestRoleID, d.GuestRoleIDs) }
```
- [ ] **Step 4: Run to verify it passes** — PASS.
- [ ] **Step 5: Commit** — `stg new config-owner-guest-ids` + refresh.

### Task 11: The `[guest]` config block

**Files:**
- Create: `internal/config/guest.go`
- Modify: `internal/config/config.go` (add `Guest Guest` field + defaults in `Load`)
- Test: `internal/config/guest_test.go`

- [ ] **Step 1: Write the failing test** — decode a TOML snippet with a `[guest]` block and assert fields + defaults.
```go
func TestGuestConfigDefaults(t *testing.T) {
	g := Guest{}.WithDefaults()
	if g.ProxyPort != "8888" || g.DindImage != "docker:dind" {
		t.Fatalf("defaults: %+v", g)
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement** `guest.go`.
```go
package config

// Guest configures the sandbox applied to non-owner (guest) sessions. The whole
// feature is inert unless [discord].guest_role_id(s) is also set.
type Guest struct {
	Image           string   `toml:"image"`             // agent container image
	ProxyImage      string   `toml:"proxy_image"`       // egress proxy image
	DindImage       string   `toml:"dind_image"`        // docker:dind
	ProxyPort       string   `toml:"proxy_port"`        // default 8888
	GitHubPAT       string   `toml:"github_pat"`        // fine-grained PAT (or via env, see Load)
	GitUserName     string   `toml:"git_user_name"`     // commit identity for guests
	GitUserEmail    string   `toml:"git_user_email"`
	EgressAllow     []string `toml:"egress_allow"`      // proxy allow-list hosts
	ModelCredMounts []string `toml:"model_cred_mounts"` // "host:container" ro binds
	AllowedTools    string   `toml:"allowed_tools"`     // claude --allowedTools for guests
	DisallowedTools string   `toml:"disallowed_tools"`  // claude --disallowedTools for guests
	DisallowedSkills []string `toml:"disallowed_skills"`
	AllowedSkills    []string `toml:"allowed_skills"`
}

func (g Guest) WithDefaults() Guest {
	if g.Image == "" {
		g.Image = "quack-sandbox:latest"
	}
	if g.ProxyImage == "" {
		g.ProxyImage = "quack-egress:latest"
	}
	if g.DindImage == "" {
		g.DindImage = "docker:dind"
	}
	if g.ProxyPort == "" {
		g.ProxyPort = "8888"
	}
	if len(g.EgressAllow) == 0 {
		g.EgressAllow = []string{"api.anthropic.com", "api.openai.com", "github.com", "api.github.com", "codeload.github.com"}
	}
	return g
}
```
  In `config.go`: add `Guest Guest `toml:"guest"`` to `Config`; in `Load`, after decode, do `cfg.Guest = cfg.Guest.WithDefaults()` and apply an env override `if v := os.Getenv("QUACK_GUEST_GITHUB_PAT"); v != "" { cfg.Guest.GitHubPAT = v }` (so the PAT need not sit in the TOML).
- [ ] **Step 4: Run to verify it passes** — PASS.
- [ ] **Step 5: Commit** — `stg new config-guest-block` + refresh.

---

## Phase 4 — Guest session path (`internal/session`)

This phase wires guests to the sandbox. Build it before role-resolution flips guests on (Phase 5), so there is never an intermediate commit where a guest gets host access.

### Task 12: The Role type and `Sandboxer` seam

**Files:**
- Create: `internal/session/role.go`
- Modify: `internal/session/service.go` (`Request` gains `Role`; new `Sandboxer` interface + `Service` field + `UseSandbox`)
- Test: `internal/session/role_test.go`

- [ ] **Step 1: Write the failing test**
```go
func TestRoleDefaultsToOwnerZeroValue(t *testing.T) {
	var r Role
	if r != RoleOwner {
		t.Fatalf("zero Role should be RoleOwner, got %v", r)
	}
	if !RoleGuest.IsGuest() {
		t.Fatal("RoleGuest.IsGuest() should be true")
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement** `role.go`.
```go
package session

// Role is the trust level of the user who issued a request. The zero value is
// RoleOwner so any code path that doesn't set a role keeps today's full-access
// behavior (owner sessions, infer/naming one-shots, tests).
type Role int

const (
	RoleOwner Role = iota
	RoleGuest
)

func (r Role) IsGuest() bool { return r == RoleGuest }
```
  In `service.go`: add `Role Role` to `Request`. Add the seam (mirrors the design's `Sandboxer`):
```go
// Sandboxer provisions and tears down the per-guest-session container set. The
// concrete adapter is internal/sandbox; session depends only on this interface
// and the plain SandboxSpec/SandboxHandle types it defines.
type Sandboxer interface {
	Provision(ctx context.Context, spec SandboxSpec) (*SandboxHandle, error)
	Teardown(ctx context.Context, h *SandboxHandle) error
	Reattach(ctx context.Context, h *SandboxHandle) error
	Launcher(h *SandboxHandle) agentproc.Launcher
}
```
  Define `SandboxSpec` and `SandboxHandle` in session as plain structs **structurally identical** to `sandbox.Spec`/`sandbox.Handle` (the adapter converts). Add `sandbox Sandboxer` and `guest GuestPolicy` fields to `Service`, plus `func (s *Service) UseSandbox(sb Sandboxer, g GuestPolicy) { s.sandbox = sb; s.guest = g }`. Define `GuestPolicy` (image/proxy/dind/PAT/git identity/egress/mounts/tool+skill policy) as a plain struct in session.

  > To keep `session` free of an import cycle, `SandboxSpec`/`SandboxHandle`/`GuestPolicy` live in `session`; the `internal/sandbox` adapter (Task 16) imports `session` and converts to/from its own `Spec`/`Handle` (same pattern as `tmuxexec` importing `session.NewSessionOpts`).
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/session/ -run TestRole` and `go build ./...` → PASS.
- [ ] **Step 5: Commit** — `stg new session-role-sandboxer-seam` + refresh.

### Task 13: Guest directive clamps

**Files:**
- Modify: `internal/session/service.go` (`run`, near the top after agent resolution ~line 200)
- Create: `internal/session/guest.go` (clamp + policy helpers)
- Test: `internal/session/guest_test.go`

- [ ] **Step 1: Write the failing test** (using the existing fake-based harness; see `fakes_test.go`). Assert: a guest directive with `Headless=false` is forced true; a guest with a filesystem-path target is rejected; `NoWorktree` is cleared.
```go
func TestClampGuestDirective(t *testing.T) {
	// no-headless forced to headless, with a note
	d := &command.Directive{Headless: false, Prompt: "x", Target: "o/r"}
	note := clampGuestDirective(d)
	if !d.Headless {
		t.Fatal("guest must be forced headless")
	}
	if note == "" {
		t.Fatal("expected a note explaining the clamp")
	}
	// no-wt cleared
	d2 := &command.Directive{Headless: true, NoWorktree: true, Target: "o/r", Prompt: "x"}
	_ = clampGuestDirective(d2)
	if d2.NoWorktree {
		t.Fatal("guest no-wt must be cleared")
	}
}

func TestGuestTargetRejectsHostPaths(t *testing.T) {
	for _, tgt := range []string{"/abs/path", "~/x", "./rel", targetTempDir} {
		if err := guestTargetAllowed(tgt); err == nil {
			t.Fatalf("target %q should be rejected for guests", tgt)
		}
	}
	for _, tgt := range []string{"", "owner/repo"} {
		if err := guestTargetAllowed(tgt); err != nil {
			t.Fatalf("target %q should be allowed for guests: %v", tgt, err)
		}
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement** `guest.go`.
```go
package session

import (
	"fmt"

	"github.com/eunomie/quack/internal/command"
	"github.com/eunomie/quack/internal/repo"
)

// clampGuestDirective normalizes a guest's directive to the safe envelope:
// always headless, never no-worktree (guests are always isolated). Returns a
// muted note when it had to override an explicit choice, "" otherwise.
func clampGuestDirective(d *command.Directive) string {
	note := ""
	if !d.Headless {
		d.Headless = true
		note = "interactive (no-headless) mode is owner-only — running headless in a sandbox instead."
	}
	d.NoWorktree = false
	return note
}

// guestTargetAllowed permits only a repo ref or no target (empty sandbox).
// Filesystem paths, ~ paths, and the temp-dir host escape are rejected — they
// would point at the host filesystem.
func guestTargetAllowed(target string) error {
	if target == "" {
		return nil
	}
	if target == targetTempDir || repo.IsPath(target) {
		return fmt.Errorf("guests can only target a repository (e.g. owner/repo), not host paths")
	}
	return nil
}
```
  In `run` (service.go), right after the agent is resolved and before `prepare`, add:
```go
	if req.Role.IsGuest() {
		if err := guestTargetAllowed(dir.Target); err != nil {
			fail(err.Error())
			return
		}
		if note := clampGuestDirective(dir); note != "" {
			_, _ = s.reply.PostSilent(ctx, threadID, mutedText("🔒 "+note))
		}
	}
```
  (Place after `threadID`/`fail` are defined; consult the current line numbers — `fail` is defined ~line 235, `threadID` ~210.)
- [ ] **Step 4: Run to verify it passes** — PASS.
- [ ] **Step 5: Commit** — `stg new session-guest-clamps` + refresh.

### Task 14: Guest workspace prep (provision the sandbox)

**Files:**
- Modify: `internal/session/service.go` (`prepare` switch; new `prepareGuest`)
- Modify: `internal/session/service.go` (`prepResult` gains the sandbox handle + launcher)
- Test: `internal/session/guest_test.go`

- [ ] **Step 1: Write the failing test** with a fake `Sandboxer` recording the spec and returning a handle; assert a repo target produces a Provision call with the resolved clone URL + ref, and that `prepResult` carries the handle and a container launcher.
```go
type fakeSandboxer struct {
	gotSpec SandboxSpec
	handle  *SandboxHandle
}

func (f *fakeSandboxer) Provision(ctx context.Context, spec SandboxSpec) (*SandboxHandle, error) {
	f.gotSpec = spec
	f.handle = &SandboxHandle{AgentContainer: "q-agent", Workdir: "/work/r"}
	return f.handle, nil
}
func (f *fakeSandboxer) Teardown(context.Context, *SandboxHandle) error { return nil }
func (f *fakeSandboxer) Reattach(context.Context, *SandboxHandle) error { return nil }
func (f *fakeSandboxer) Launcher(h *SandboxHandle) agentproc.Launcher {
	return agentproc.ContainerLauncher{Container: h.AgentContainer, Workdir: h.Workdir}
}

func TestPrepareGuestProvisionsSandboxForRepo(t *testing.T) {
	s := newTestService(t)           // helper in fakes_test.go style
	fs := &fakeSandboxer{}
	s.UseSandbox(fs, GuestPolicy{Image: "img", GitHubPAT: "PAT"})
	dir := &command.Directive{Target: "owner/repo", Prompt: "x"}
	prep, err := s.prepareGuest(context.Background(), dir, "prov", "name")
	if err != nil {
		t.Fatal(err)
	}
	if fs.gotSpec.RepoURL == "" {
		t.Fatal("expected a clone URL in the spec")
	}
	if prep.launcher == nil || prep.sandbox == nil {
		t.Fatalf("prep missing sandbox/launcher: %+v", prep)
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement** — extend `prepResult`:
```go
	sandbox  *SandboxHandle      // guest only; nil for owner
	launcher agentproc.Launcher  // guest only; nil => DirectLauncher
```
  Add a role branch at the top of `prepare`:
```go
func (s *Service) prepare(ctx context.Context, dir *command.Directive, provisional string, explicit bool, token, suggested string, role Role) (prepResult, error) {
	if role.IsGuest() {
		return s.prepareGuest(ctx, dir, provisional, orDefault(suggested, provisional))
	}
	switch dir.Target { /* …unchanged… */ }
}
```
  (Thread `role` from `run` into `prepare`.) Implement `prepareGuest`:
```go
// prepareGuest provisions an isolated Docker sandbox for a guest. A repo target
// is cloned fresh inside the container; no target yields an empty sandbox. The
// clone URL/ref are resolved on the host (origin only) so no owner local-only
// branches leak in.
func (s *Service) prepareGuest(ctx context.Context, dir *command.Directive, provisional, name string) (prepResult, error) {
	spec := SandboxSpec{
		SessionName:  name,
		GitHubPAT:    s.guest.GitHubPAT,
		GitUserName:  s.guest.GitUserName,
		GitUserEmail: s.guest.GitUserEmail,
		EgressAllow:  s.guest.EgressAllow,
		ModelMounts:  s.guest.ModelMounts,
	}
	label := ""
	if dir.Target != "" {
		ref, err := repo.ParseRef(dir.Target)
		if err != nil {
			return prepResult{}, err
		}
		spec.RepoURL = ref.CloneURL("https") // HTTPS so the injected PAT authenticates; no SSH key in the jail
		spec.CloneRef = dir.Base             // "" => default branch (clone uses remote HEAD)
		spec.RepoDir = ref.Repo
		label = ref.Owner + "/" + ref.Repo
	}
	h, err := s.sandbox.Provision(ctx, spec)
	if err != nil {
		return prepResult{}, fmt.Errorf("provision sandbox: %w", err)
	}
	return prepResult{
		workdir:  h.Workdir, // in-container path; only used by the container launcher
		name:     name,
		isolated: true,
		label:    label,
		sandbox:  h,
		launcher: s.sandbox.Launcher(h),
	}, nil
}
```
  > `CloneURL("https")` must produce an HTTPS URL even when `clone_protocol = ssh`; verify `repo.CloneURL` honors the explicit "https" arg (it takes the protocol as a parameter per `prepareFromRef`). If the default branch is needed, omit `--branch` (Task 7 already does when `CloneRef==""`), letting `git clone` use the remote's HEAD.
- [ ] **Step 4: Run to verify it passes** — PASS. Update existing `prepare` callers for the new `role` arg; `go build ./...`.
- [ ] **Step 5: Commit** — `stg new session-prepare-guest` + refresh.

### Task 15: Carry launcher + guest driver into the headless session

**Files:**
- Modify: `internal/session/service.go` (`run` → `startHeadless` call passes launcher + handle)
- Modify: `internal/session/headless.go` (`liveSession` gains `launcher`, `sandbox`; `startHeadless` signature; `runTurn` passes `Launcher`; teardown calls `Teardown`)
- Modify: `internal/session/guest.go` (build the guest-specific driver)
- Test: `internal/session/guest_test.go`

- [ ] **Step 1: Write the failing test** — assert a guest headless session runs its turn through the container launcher (fake driver records the `Turn.Launcher`), and that stopping it tears the sandbox down (fake `Sandboxer.Teardown` called).
```go
func TestGuestTurnUsesContainerLauncherAndTeardown(t *testing.T) {
	// Build a guest liveSession via startHeadless with a recording driver +
	// fake sandboxer; enqueue one turn; assert the driver saw a ContainerLauncher;
	// StopThread → Teardown called exactly once.
}
```
  (Flesh out using the existing fake driver in `fakes_test.go`; record `tr.Launcher` type.)
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement.**
  - `liveSession`: add `launcher agentproc.Launcher` and `sandbox *SandboxHandle`.
  - `runTurn` (headless.go:368): add `Launcher: ls.launcher,` to the `agentproc.Turn{…}`.
  - `startHeadless`: take `launcher agentproc.Launcher`, `handle *SandboxHandle`, and a per-session `driver agentproc.Driver` (guest-configured) override; store them on the `liveSession` built by `newSession`. Change `newSession`/`sessionRecord` plumbing minimally — pass these through (the record itself is extended in Task 16).
  - In `run`, when launching headless for a guest, build the guest driver and pass the launcher/handle:
```go
	var launcher agentproc.Launcher
	var handle *SandboxHandle
	driverName := agentName
	if req.Role.IsGuest() {
		launcher = prep.launcher
		handle = prep.sandbox
	}
	// startHeadless picks s.drivers[agentName] by default; for guests, override
	// with a tool-restricted driver (Task 18 supplies guestDriver()).
```
  - `StopThread`/`PromoteThread`: after `ls.close()` and record removal, if `ls.sandbox != nil` call `s.sandbox.Teardown(ctx, ls.sandbox)`. (Guests never promote — `PromoteThread` should refuse a guest session with a note; gate on `ls.sandbox != nil`.)
- [ ] **Step 4: Run to verify it passes** — PASS; `go build ./...`.
- [ ] **Step 5: Commit** — `stg new session-guest-headless-wiring` + refresh.

### Task 16: Persist + rehydrate guest sessions

**Files:**
- Modify: `internal/session/persist.go` (`sessionRecord` + `record()` + `newSession` + `Rehydrate`)
- Modify: `internal/sandbox` (adapter conversion `session.SandboxHandle` ⇄ `sandbox.Handle`, if not already structural)
- Test: `internal/session/persist_test.go`

- [ ] **Step 1: Write the failing test** — a guest record round-trips `Role` + `Sandbox` handle; `Rehydrate` of a guest record calls `Reattach` and rebuilds the container launcher; a record whose work volume is gone is skipped.
```go
func TestRehydrateGuestReattachesSandbox(t *testing.T) {
	// Persist a guest sessionRecord (Role=RoleGuest, Sandbox handle set);
	// Rehydrate → fake Sandboxer.Reattach called; liveSession.launcher is a
	// ContainerLauncher.
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement.**
  - `sessionRecord`: add `Role Role `json:"role"`` and `Sandbox *SandboxHandle `json:"sandbox,omitempty"``.
  - `record()`: include both from the `liveSession`.
  - `newSession`: if `rec.Role.IsGuest()` and `rec.Sandbox != nil`, call `s.sandbox.Reattach(ctx, rec.Sandbox)` and set `ls.launcher = s.sandbox.Launcher(rec.Sandbox)`, `ls.sandbox = rec.Sandbox`, and `ls.driver = s.guestDriver(rec.AgentName)` (Task 18). For owners, unchanged (`ls.launcher` nil → DirectLauncher).
  - `Rehydrate`: for guest records, replace the `s.git.PathExists(rec.Workdir)` liveness check (a guest's workdir is an in-container path, not a host path) with `Reattach` success — if `Reattach` errors (work volume gone), skip the record and best-effort `Teardown` the remnants.
- [ ] **Step 4: Run to verify it passes** — PASS.
- [ ] **Step 5: Commit** — `stg new session-guest-persist-rehydrate` + refresh.

---

## Phase 5 — Role resolution & own-session-only (`internal/discord`)

### Task 17: Resolve owner/guest and set Request.Role

**Files:**
- Modify: `internal/discord/bot.go` (`Allow` gains `OwnerUserIDs`, `GuestRoleIDs`; new `resolveRole`; `onMessage` sets role; `authorized` accepts owners+guests)
- Test: `internal/discord/bot_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**
```go
func TestResolveRole(t *testing.T) {
	b := &Bot{allowed: Allow{
		OwnerUserIDs: []string{"owner"},
		GuestRoleIDs: []string{"guildrole"},
		GuildIDs:     []string{"g"},
	}}
	// owner id → owner, authorized
	if role, ok := b.resolveRole("owner", "g", "chan", nil); !ok || role != session.RoleOwner {
		t.Fatalf("owner: role=%v ok=%v", role, ok)
	}
	// member with the guest role → guest, authorized
	if role, ok := b.resolveRole("someone", "g", "chan", []string{"guildrole"}); !ok || role != session.RoleGuest {
		t.Fatalf("guest: role=%v ok=%v", role, ok)
	}
	// no owner id, no guest role → rejected
	if _, ok := b.resolveRole("nobody", "g", "chan", []string{"other"}); ok {
		t.Fatal("should be rejected")
	}
	// guest in a non-allowed guild → rejected
	if _, ok := b.resolveRole("someone", "other-guild", "chan", []string{"guildrole"}); ok {
		t.Fatal("guest outside allowed guild should be rejected")
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement.** Extend `Allow` with `OwnerUserIDs []string` and `GuestRoleIDs []string`. Add:
```go
// resolveRole decides a user's trust level. Owners (by id) get full access;
// otherwise a member holding a guest role, inside an allowed guild+channel, is a
// guest. Returns ok=false when neither applies (request is rejected).
func (b *Bot) resolveRole(userID, guildID, channelID string, memberRoles []string) (session.Role, bool) {
	if allows(b.allowed.OwnerUserIDs, userID) && len(b.allowed.OwnerUserIDs) > 0 {
		// owner_user_ids set and matched → owner; if no owners configured, fall through
	}
	for _, id := range b.allowed.OwnerUserIDs {
		if id == userID {
			return session.RoleOwner, true
		}
	}
	if !allows(b.allowed.GuildIDs, guildID) || !allows(b.allowed.ChannelIDs, channelID) {
		return 0, false
	}
	for _, want := range b.allowed.GuestRoleIDs {
		for _, have := range memberRoles {
			if want == have {
				return session.RoleGuest, true
			}
		}
	}
	return 0, false
}
```
  > Note: `allows` treats an empty owner list as "any". Use an explicit loop for owners (above) so an empty `OwnerUserIDs` does **not** silently make everyone an owner. Keep `allows` for the guild/channel gate (empty = any) to preserve current semantics.

  In `onMessage`, replace the `b.authorized(m)` block with:
```go
	role, ok := b.resolveRole(m.Author.ID, m.GuildID, m.ChannelID, memberRoleIDs(m.Member))
	if !ok {
		_, _ = s.ChannelMessageSend(m.ChannelID, "🦆 not authorized")
		return
	}
	// …build req…
	req.Role = role
```
  Add a tiny helper `memberRoleIDs(*discordgo.Member) []string` returning `m.Roles` (nil-safe).
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/discord/` → PASS; `go build ./...`.
- [ ] **Step 5: Commit** — `stg new discord-resolve-role` + refresh.

### Task 18: Guest-restricted driver (tools/skills)

**Files:**
- Modify: `internal/session/guest.go` (`guestDriver`, `GuestPolicy` tool/skill fields)
- Modify: `cmd/quack/main.go` later (Task 20 supplies the base claude config)
- Test: `internal/session/guest_test.go`

- [ ] **Step 1: Write the failing test** — `guestDriver` returns a claude driver whose `AllowedTools`/`DisallowedTools`/`Settings` reflect the guest policy (block `open-zed`, allow `revue`), per the matcher confirmed in P3.
```go
func TestGuestDriverAppliesToolPolicy(t *testing.T) {
	s := newTestService(t)
	s.UseSandbox(&fakeSandboxer{}, GuestPolicy{
		DisallowedSkills: []string{"open-zed"},
		AllowedSkills:    []string{"revue"},
	})
	// register a base claude driver under "claude"
	d := s.guestDriver("claude")
	c, ok := d.(agentproc.Claude)
	if !ok {
		t.Fatalf("expected claude driver, got %T", d)
	}
	if !strings.Contains(c.DisallowedTools, "open-zed") {
		t.Fatalf("guest claude must disallow open-zed: %q", c.DisallowedTools)
	}
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL (no `guestDriver`, no `DisallowedTools` on `Claude`).
- [ ] **Step 3: Implement.**
  - Add `DisallowedTools string` to `agentproc.Claude` and emit `--disallowedTools` in `args` (mirroring the existing `AllowedTools` block, claude.go:30-31). Codex has no equivalent — `guestDriver` returns the codex driver unchanged.
  - `guestDriver(agentName)`: start from `s.drivers[agentName]`; if it's `agentproc.Claude`, return a copy with `AllowedTools`/`DisallowedTools` (and any `Settings`) derived from `s.guest` policy via a small `claudeSkillFlags(policy)` helper that encodes P3's matcher (e.g. each disallowed skill → `Skill(<name>)`). Non-claude drivers returned as-is.
- [ ] **Step 4: Run to verify it passes** — PASS.
- [ ] **Step 5: Commit** — `stg new session-guest-driver-tools` + refresh.

### Task 19: Own-session-only for guest feed/stop/react

**Files:**
- Modify: `internal/session/headless.go` (`FeedThread`/`StopThread`/`StopByMessage` accept the caller's role+id; enforce author match for guests)
- Modify: `internal/session/headless.go` (`liveSession` stores `authorID`; set from `Origin.AuthorID`; persist it)
- Modify: `internal/discord/bot.go` (thread + reaction paths resolve role and pass it; reaction path reads `r.Member.Roles`)
- Test: `internal/session/guest_test.go`, `internal/discord/bot_test.go`

- [ ] **Step 1: Write the failing test** — a guest may not feed/stop a session started by another author; the owner may; the original guest may.
```go
func TestGuestCannotTouchOthersSession(t *testing.T) {
	// liveSession with authorID="alice"
	// FeedThread as guest "bob" → false/ignored; as guest "alice" → true; as owner → true.
}
```
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement.**
  - `liveSession` + `sessionRecord`: add `authorID` / `AuthorID` (set from `Origin.AuthorID` at `startHeadless`; persist).
  - Add a caller identity to the mutating entry points: `FeedThread(ctx, threadID, channelID, messageID, text, atts, caller Caller)` where `type Caller struct { Role Role; UserID string }`. At the top: `if caller.Role.IsGuest() && ls.authorID != caller.UserID { return false }`. Same guard in `StopThread`/`StopByMessage` (resolve the session first, then check).
  - `bot.go`: in the tracked-thread branch and `onReaction`, resolve the role (`resolveRole`) and pass a `session.Caller`. For reactions use `r.Member.Roles` (guild reactions include the member); if `r.Member` is nil, fetch via `s.State.Member`/`GuildMember` (reuse the pattern in `resolveBotRoles`).
- [ ] **Step 4: Run to verify it passes** — PASS; `go build ./...`; full `go test ./...`.
- [ ] **Step 5: Commit** — `stg new session-guest-own-session-only` + refresh.

---

## Phase 6 — Wiring, config docs, and the adapter conversion

### Task 20: Adapter + main.go wiring

**Files:**
- Create: `internal/sandbox/adapter.go` (implement `session.Sandboxer` over `DockerProvisioner`; convert `session.SandboxSpec`/`SandboxHandle` ⇄ `sandbox.Spec`/`Handle`)
- Modify: `cmd/quack/main.go`
- Test: `internal/sandbox/adapter_test.go`

- [ ] **Step 1: Write the failing test** — the adapter maps a `session.SandboxSpec` to a `sandbox.Spec` and back without losing fields (table test).
- [ ] **Step 2: Run to verify it fails** — FAIL.
- [ ] **Step 3: Implement** `adapter.go`:
```go
package sandbox

import (
	"context"

	"github.com/eunomie/quack/internal/agentproc"
	"github.com/eunomie/quack/internal/session"
)

// Adapter implements session.Sandboxer over a DockerProvisioner.
type Adapter struct{ P *DockerProvisioner }

func (a Adapter) Provision(ctx context.Context, s session.SandboxSpec) (*session.SandboxHandle, error) {
	h, err := a.P.Provision(ctx, toSpec(s))
	if err != nil {
		return nil, err
	}
	return toSessionHandle(h), nil
}
// Teardown/Reattach/Launcher likewise convert. Launcher delegates to a.P.Launcher.
func (a Adapter) Launcher(h *session.SandboxHandle) agentproc.Launcher {
	return a.P.Launcher(fromSessionHandle(h))
}
```
  (Write the `toSpec`/`toSessionHandle`/`fromSessionHandle` converters — straight field copies.)
  In `main.go`: after building `drivers`, if `len(cfg.Discord.GuestRoles()) > 0`, build the provisioner and guest policy and call `svc.UseSandbox(...)`:
```go
	if len(cfg.Discord.GuestRoles()) > 0 {
		prov := &sandbox.DockerProvisioner{
			D: sandbox.NewDocker(),
			AgentImage: cfg.Guest.Image, ProxyImage: cfg.Guest.ProxyImage,
			DindImage: cfg.Guest.DindImage, ProxyPort: cfg.Guest.ProxyPort,
		}
		svc.UseSandbox(sandbox.Adapter{P: prov}, session.GuestPolicy{
			GitHubPAT: cfg.Guest.GitHubPAT, GitUserName: cfg.Guest.GitUserName,
			GitUserEmail: cfg.Guest.GitUserEmail, EgressAllow: cfg.Guest.EgressAllow,
			ModelMounts: parseMounts(cfg.Guest.ModelCredMounts),
			AllowedTools: cfg.Guest.AllowedTools, DisallowedTools: cfg.Guest.DisallowedTools,
			DisallowedSkills: cfg.Guest.DisallowedSkills, AllowedSkills: cfg.Guest.AllowedSkills,
		})
	}
```
  Update `discord.Allow{…}` construction to include `OwnerUserIDs: cfg.Discord.OwnerIDs()` and `GuestRoleIDs: cfg.Discord.GuestRoles()`. (`UserIDs` stays for the legacy/guild/channel gate.)
- [ ] **Step 4: Run to verify it passes** — PASS; `go build ./cmd/quack`.
- [ ] **Step 5: Commit** — `stg new sandbox-adapter-mainwiring` + refresh.

### Task 21: Config example + AGENTS.md

**Files:**
- Modify: `config.example.toml`
- Modify: `AGENTS.md`

- [ ] **Step 1:** Add to `config.example.toml`: the `owner_user_id(s)` / `guest_role_id(s)` lines under `[discord]` (with comments explaining owner=full, guest=sandbox, and that legacy `allowed_user_id(s)` = owner), and a full commented `[guest]` block (image names, `github_pat` + the `QUACK_GUEST_GITHUB_PAT` env alternative, `egress_allow`, `model_cred_mounts`, `disallowed_skills`/`allowed_skills`).
- [ ] **Step 2:** Add an "## Access control & guest sandboxes" section to `AGENTS.md`: the owner/guest model, that guests run in `docker:dind`-sidecar sandboxes (forced headless, repo-only, egress-allow-listed, fine-grained PAT for push), the build/setup steps (`hack/sandbox`), and the threat model summary (sandbox protects host + owner's other data; shared creds are scoped/revocable, not unreachable).
- [ ] **Step 3: Commit** — `stg new docs-guest-config` + refresh.

### Task 22: Full verification

- [ ] **Step 1:** `go build ./... && go vet ./... && go test ./...` → all PASS.
- [ ] **Step 2:** Build images (`docker build -t quack-sandbox:latest hack/sandbox`, `docker build -t quack-egress:latest hack/sandbox/proxy`).
- [ ] **Step 3:** `QUACK_INTEGRATION=1 go test ./internal/sandbox/...` → PASS (real dind sibling + egress + teardown).
- [ ] **Step 4:** Manual end-to-end on a test guild: a guest mention → sandboxed session that can `docker run hello-world` and `git push` a branch via the PAT; an owner mention → unchanged host session; a guest `no-headless` → forced headless with the note; a guest host-path target → rejected; a guest stop reaction on another guest's session → ignored.
- [ ] **Step 5:** Reorganize the patch series if needed (`/organize-patches`) so the stack tells a clean story (launcher seam → sandbox pkg → config → guest session → role resolution → wiring/docs), then stop (no push without explicit approval).

---

## Self-review (run after drafting; fixes already folded in)

**Spec coverage:**
- Roles (owner by id, guest by role) → Tasks 10, 17. ✅
- Guest sandbox: agent + dind sidecar + private net → Tasks 7, 8, 9; design §2. ✅
- Egress allow-list → Tasks 4, 7 (proxy on internal net; agent has no other route). ✅
- Credentials minimal/injected (model file ro, git env, PAT) → Tasks 7, 11, P1. ✅
- Workspace rules (repo→clone, none→empty, host paths/temp-dir/no-wt rejected) → Tasks 13, 14. ✅
- Forced headless → Task 13. ✅
- Tools/skills (block zed, allow revue) → Tasks 18, P3. ✅
- Launcher seam, owner path unchanged → Tasks 1–3. ✅
- Own-session-only for guests → Task 19. ✅
- Restart resilience (role + handle persisted, reattach) → Task 16. ✅
- Config shape + docs → Tasks 10, 11, 21. ✅
- Threat model documented → Task 21. ✅

**Placeholder scan:** the only deferred specifics are the four P-tasks (credential paths, egress hosts, skill matcher, dind/TLS), each an explicit spike that records a concrete answer before the dependent task — not in-code placeholders.

**Type consistency:** `Launcher.Command(ctx, program, args, dir, env)` is used identically in Tasks 1–3, 8, 18. `SandboxSpec`/`SandboxHandle` (session) ↔ `Spec`/`Handle` (sandbox) are converted only in the Task 20 adapter. `Role` (zero = owner) is consistent across Tasks 12, 16, 17, 19. `Sandboxer` interface (Provision/Teardown/Reattach/Launcher) matches between Task 12 (definition), Task 14/16 (use), and Task 20 (impl).
