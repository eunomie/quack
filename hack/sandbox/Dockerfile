# quack guest-sandbox agent image.
#
# This is the image the *agent* container runs (the unprivileged box a guest's
# claude/codex turns are `docker exec`'d into). It needs: git + gh + stg
# (commit/patch/push via the injected fine-grained PAT), the Docker *client*
# (talks to the dind sidecar over DOCKER_HOST, never the host socket), a JS
# runtime for claude, and the claude + codex CLIs.
#
# Build:  docker build -t quack-sandbox:latest hack/sandbox
#
# NOTE: the agent-CLI install lines below are finalized during the host-
# verification step (spike P1) against the vendors' current distribution
# channels — adjust the exact package/registry names there if they have moved.
FROM debian:bookworm-slim

# Base tooling. docker.io provides the docker client (the daemon binary is
# present but unused — the agent talks to the dind sidecar, not a local daemon).
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates git curl gnupg docker.io openssh-client build-essential stgit \
    && rm -rf /var/lib/apt/lists/*

# GitHub CLI (gh) from its official apt repo.
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y gh \
    && rm -rf /var/lib/apt/lists/*

# Node + the agent CLIs. (Channels verified in spike P1.)
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && npm i -g @anthropic-ai/claude-code @openai/codex \
    && rm -rf /var/lib/apt/lists/*

# dagger CLI. Its engine runs inside the dind sidecar (the agent's DOCKER_HOST
# points there), so guests can run dagger pipelines; sharing the owner's dagger
# Cloud auth is handled at provision time (see [guest].cred_files). Installs the
# latest release by default (empty DAGGER_VERSION); pin a specific version with
# --build-arg DAGGER_VERSION=v0.21.4 .
ARG DAGGER_VERSION=
RUN curl -fsSL https://dl.dagger.io/dagger/install.sh | BIN_DIR=/usr/local/bin DAGGER_VERSION="$DAGGER_VERSION" sh

WORKDIR /work
