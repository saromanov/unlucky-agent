#!/usr/bin/env bash
# One-shot bootstrap for unlucky-agent: create the GitHub repository, push the
# initial skeleton and open tracking issues. Runs unattended at 03:00 from cron
# and removes its own crontab entry on success or on a fatal error.
set -euo pipefail

OWNER="saromanov"
NAME="unlucky-agent"
REPO_DIR="/home/sergey/saromanov/unlucky-agent"
LOG_DIR="/home/sergey/hub/logs"
MARKER="unlucky-agent/bootstrap-3am"

export PATH="/usr/local/bin:/usr/bin:/bin:/usr/local/go/bin"
# Cron starts with a minimal environment: GOPATH must point at the real module
# cache, otherwise Go tries to download modules from the network.
export GOPATH="${GOPATH:-/home/sergey/gowork}"
export GOMODCACHE="$GOPATH/pkg/mod"
export GOCACHE="${GOCACHE:-$HOME/.cache/go-build}"

log() { printf '%s %s\n' "$(date -Is)" "$*"; }

remove_cron() {
	crontab -l 2>/dev/null | grep -v "$MARKER" | crontab - || true
}

fatal() {
	# Keep the crontab entry: the next nightly run retries. Removing it here
	# would silently lose the bootstrap after a transient failure.
	log "FATAL: $*"
	exit 1
}

cd "$REPO_DIR" || fatal "repository directory not found"

if [ "${1:-}" = "--dry-run" ]; then
	log "dry run"
	command -v gh >/dev/null || fatal "gh not found"
	command -v git >/dev/null || fatal "git not found"
	gh auth status >/dev/null 2>&1 || fatal "gh is not authenticated"
	log "branch: $(git rev-parse --abbrev-ref HEAD)"
	log "commits: $(git rev-list --count HEAD), dirty: $(git status --porcelain | wc -l)"
	log "issues planned: $(ls ops/issues/*.md | wc -l)"
	exit 0
fi

mkdir -p "$LOG_DIR"
log "start"

git rev-parse --git-dir >/dev/null 2>&1 || fatal "not a git repository"

build_log="$(go build ./... 2>&1)" || fatal "go build failed: $build_log"
log "build ok"

test_log="$(go test ./... 2>&1)" || fatal "go test failed: $test_log"
log "tests ok"

if gh repo view "$OWNER/$NAME" >/dev/null 2>&1; then
	log "repository exists: $OWNER/$NAME"
else
	log "creating repository $OWNER/$NAME"
	# Publish over HTTPS: no SSH key is configured for GitHub on this host.
	gh repo create "$OWNER/$NAME" --public \
		--description "Reproducible crash-test bench for AI agent rollback tools" \
		|| fatal "repository creation failed"
fi

if ! git remote get-url origin >/dev/null 2>&1; then
	git remote add origin "https://github.com/$OWNER/$NAME.git"
else
	git remote set-url origin "https://github.com/$OWNER/$NAME.git"
fi
git config --local credential.helper '!gh auth git-credential'
git push -u origin HEAD:main || fatal "push failed"

existing="$(gh issue list --repo "$OWNER/$NAME" --state all --limit 200 \
	--json title --jq '.[].title' || true)"

for f in ops/issues/*.md; do
	title="$(head -n1 "$f" | sed 's/^# //')"
	if printf '%s\n' "$existing" | grep -Fxq "$title"; then
		log "issue exists: $title"
		continue
	fi
	body="$(tail -n +2 "$f")"
	if gh issue create --repo "$OWNER/$NAME" --title "$title" --body "$body" >/dev/null; then
		log "issue created: $title"
	else
		log "WARN issue failed: $title"
	fi
done

log "done: https://github.com/$OWNER/$NAME"
remove_cron
