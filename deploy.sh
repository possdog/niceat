#!/bin/sh
# Deploy niceat to Fly.io in one command.
#
#   - syncs the instance secrets (config + Porkbun credentials),
#   - builds and ships the serving Machine (fly deploy),
#   - recreates the scheduled reaper Machine on the image that deploy
#     just released, so it always runs the latest code.
#
# One-time setup is deliberately NOT here (fly apps create, fly certs
# add, the DNS records they print) — see AGENTS.md "Deployment notes".
#
# Requires: an authenticated fly CLI, jq, and niceat.json +
# porkbun_keys.json in the repo root (both gitignored).
set -eu
cd "$(dirname "$0")"

APP=possdog-niceat
REGION=iad

[ -f niceat.json ] || { echo "missing niceat.json (copy niceat.example.json)" >&2; exit 1; }
[ -f porkbun_keys.json ] || { echo "missing porkbun_keys.json (copy porkbun_keys.example.json)" >&2; exit 1; }

# base64 flags differ: BSD (macOS) uses -i, GNU (Linux) -w0.
case "$(uname)" in
	Darwin) B64="base64 -i" ;;
	*) B64="base64 -w0" ;;
esac

# Secrets sync on every deploy so local config edits propagate.  Without
# --stage this rolls the new environment out to the app's machines itself;
# fly deploy below then applies the new image on top.
fly secrets set -a "$APP" \
	"NICEAT_CONFIG=$($B64 niceat.json)" \
	"PORKBUN_CREDENTIALS=$(cat porkbun_keys.json)"

# Serving Machine: build, release, apply (region and health checks come
# from fly.toml).
fly deploy -a "$APP"

# Reaper Machine.  Recreated — not updated — on every deploy so its
# image, environment, and config file are all guaranteed current: a
# Machine created through the Machines API is otherwise never touched by
# `fly deploy`.  Recreating drops reaper_state.json, which only restarts
# grace clocks (reaps are delayed, never accelerated — the fail-safe
# direction by design).  Its creation-time run doubles as a per-deploy
# smoke test: watch `fly logs -a $APP` for "reaper sweep complete".
OLD=$(fly machines list -a "$APP" --json |
	jq -r '[.[]
		| select((.config.metadata["fly_process_group"] // "") != "app")
		| .id] | join(" ")')
for id in $OLD; do
	fly machines destroy -a "$APP" --force "$id"
done

IMAGE=$(fly releases -a "$APP" --image --json | jq -r '.[0].ImageRef')
fly machines run -a "$APP" \
	--region "$REGION" \
	--restart=no \
	--schedule=daily \
	--metadata "fly_process_group=reaper" \
	--file-local "/app/niceat.json=$PWD/niceat.json" \
	"$IMAGE" -- -reap-once

fly machines list -a "$APP"
