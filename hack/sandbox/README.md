# Guest sandbox images

quack confines **guest** (non-owner) sessions to per-session Docker sandboxes.
Two images back that:

| Image | Built from | Role |
|-------|-----------|------|
| `quack-sandbox:latest` | `hack/sandbox/Dockerfile` | the **agent** container — runs `claude`/`codex`, holds the per-session clone + injected creds, talks to the dind sidecar |
| `quack-egress:latest` | `hack/sandbox/proxy/Dockerfile` | the **egress proxy** — allow-list CONNECT proxy the agent's `HTTPS_PROXY` points at (model API + GitHub only) |

The `docker:dind` sidecar image is pulled directly from Docker Hub (configurable
via `[guest].dind_image`).

## Build (setup prerequisite)

```sh
docker build -t quack-sandbox:latest hack/sandbox
docker build -t quack-egress:latest  hack/sandbox/proxy
```

Image names are overridable in config (`[guest].image`, `[guest].proxy_image`).

## How a guest session is wired

Per session quack creates: a private **internal** network (the agent has *no*
direct internet route), an external network, a certs volume + a work volume, the
egress proxy, a privileged `docker:dind` sidecar, and the unprivileged agent
container. Turns run as `docker exec` into the agent container; `/stop` (or
thread archive) tears the whole set down. See
`hack/designs/2026-06-05-multi-user-sandbox-hardening.md` for the full design and
threat model.

## Security note

The sandbox protects the **host and the owner's other data** — a guest agent
cannot read the owner's SSH keys, other repos, or the quack config, because none
of it is in the container. It does **not** make the deliberately-shared
credentials (the model auth and the fine-grained GitHub PAT) unreachable; those
live in the agent container by necessity and are contained by being **scoped and
revocable**, not unreachable. The dind sidecar is privileged — that is the
residual host-boundary risk the owner accepted (over the unsupported-on-Fedora
Sysbox runtime).
