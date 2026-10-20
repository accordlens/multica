#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
  scripts)
    node --test scripts/*.test.mjs
    for suite in scripts/*.test.sh; do bash "$suite"; done
    git fetch --no-tags origin main
    node scripts/check-image-budget.mjs --base FETCH_HEAD
    node scripts/check-ui-wildcard-exports.mjs
    node scripts/check-ui-radius-tokens.mjs
    node scripts/generate-reserved-slugs.mjs
    git diff --exit-code -- packages/core/paths/reserved-slugs.ts
    ;;
  sqlc)
    make sqlc
    git diff --exit-code -- server/pkg/db/generated
    test -z "$(git ls-files --others --exclude-standard -- server/pkg/db/generated)"
    ;;
  backend-build)
    (cd server && go build -race ./... && go vet -race -tags agentintegration ./...)
    make build
    ;;
  backend-test)
    (cd server && go run ./cmd/migrate up)
    # Run every package with race detection; serial tests avoid starving the
    # subprocess deadlines on developer machines. Never replay cached results.
    bash scripts/go-test-with-agent-cli-guard.sh -- bash -c \
      'cd server && go test -race -count=1 -p=4 -parallel=1 ./...'
    ;;
  vulnerabilities) (cd server && go tool govulncheck ./...) ;;
  selfhost)
    pwsh -NoProfile -File scripts/selfhost-release.ps1 -Platform linux/amd64 -OutputDirectory release-artifacts/local-ci -Smoke
    pwsh -NoProfile -File scripts/selfhost-data.test.ps1 -ReleaseEnv release-artifacts/local-ci/release.env
    ;;
  *) echo 'Unknown local CI gate' >&2; exit 2 ;;
esac
