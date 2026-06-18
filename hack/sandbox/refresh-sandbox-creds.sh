#!/usr/bin/env bash
# Refresh the injected credentials on live guest sandboxes by recreating their
# containers — picking up an edited PAT / bot token / cred files from config
# WITHOUT losing the agent's work or conversation history.
#
# Why this exists: the GitHub PAT (GH_TOKEN) and the Discord broker's bot token
# are baked into their containers as env vars at `docker run` time, and the
# shared cred files are copied in at provision time. None can be changed on a
# running container, and a plain quack restart only `Start`s existing containers
# (Reattach) — it does NOT re-inject anything. Creds are re-sourced from current
# config only when `bringUp` runs, i.e. when Reattach finds the agent container
# GONE and rebuilds the set.
#
# `bringUp` is NOT idempotent against surviving siblings: it does
# `docker network create` and `docker run --name` for the networks and the
# proxy/dind/discord containers, which collide if those still exist. So we remove
# the per-session CONTAINERS and NETWORKS, but KEEP every volume:
#
#   removed:  -agent, -dind, -proxy, -discord containers; -int, -ext networks
#   kept:     -work (clone), -home (claude/codex history), -certs volumes
#
# The -home volume is why this is now non-destructive: since the agent's $HOME
# lives on a persistent volume, recreating its container no longer wipes the
# conversation history (it did before that volume existed). `docker volume create`
# is idempotent, so bringUp reuses the kept volumes rather than re-creating them.
#
# Then a detached quack restart triggers Reattach -> container gone -> bringUp,
# which recreates the containers and re-injects the current creds, with the work
# and history volumes intact.
#
# Usage:
#   refresh-sandbox-creds.sh                 # refresh all guest sandboxes
#   refresh-sandbox-creds.sh <session>...    # refresh only the named session(s)
#
# Uses a detached systemd-run restart so it fires even when this script is run
# from inside a quack-spawned session (which lives in the quack.service cgroup).
set -euo pipefail

restart_delay=10   # seconds; lets this turn/script finish before quack restarts

# Resolve the set of session prefixes ("quack-<n>-") to refresh, from their
# agent containers.
prefixes=()
if [[ $# -gt 0 ]]; then
	for s in "$@"; do
		prefixes+=("quack-${s}-")
	done
else
	while IFS= read -r name; do
		[[ -n "$name" ]] && prefixes+=("${name%agent}")   # strip trailing "agent", keep "quack-<n>-"
	done < <(docker ps -a --filter 'name=quack-' --format '{{.Names}}' | grep -- '-agent$' || true)
fi

if [[ ${#prefixes[@]} -eq 0 ]]; then
	echo "No quack guest agent containers found — nothing to refresh." >&2
	exit 0
fi

echo "Will recreate these guest sandboxes (all volumes — work, history, certs — preserved):"
printf '  %s*\n' "${prefixes[@]}"
echo

for p in "${prefixes[@]}"; do
	echo "== ${p}* =="
	# Containers first (a network can't be removed while containers are attached).
	for c in "${p}agent" "${p}dind" "${p}proxy" "${p}discord"; do
		if docker inspect --type container "$c" >/dev/null 2>&1; then
			echo "  rm container $c"
			docker rm -f "$c" >/dev/null
		fi
	done
	# Networks next. Volumes are KEPT (bringUp reuses them; history/clone survive).
	for n in "${p}int" "${p}ext"; do
		if docker network inspect "$n" >/dev/null 2>&1; then
			echo "  rm network   $n"
			docker network rm "$n" >/dev/null
		fi
	done
	for v in "${p}work" "${p}home" "${p}certs"; do
		if docker volume inspect "$v" >/dev/null 2>&1; then
			echo "  keep volume  $v"
		fi
	done
	echo
done

echo "Scheduling a detached quack restart in ${restart_delay}s so Reattach rebuilds with refreshed creds..."
systemd-run --user --on-active="${restart_delay}" systemctl --user restart quack.service
echo "Done. quack will restart shortly and re-provision each sandbox from current config."
