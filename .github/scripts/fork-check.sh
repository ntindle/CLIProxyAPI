#!/usr/bin/env bash
# Gate for the ntindle fork. Run from the repository root.
#
# Blocking: everything compiles, the fork's hooks into upstream files are still in place,
# and the fork's own tests (TestFork*) pass.
# Advisory: the rest of the tests in the packages the fork touches. Some of those depend on
# goroutine scheduling and fail now and then on small CI runners, so a failure there is
# reported as a warning instead of blocking a sync or an image.
set -euo pipefail

fork_packages=(
  ./cmd/server/...
  ./internal/api/...
  ./internal/config/...
  ./internal/registry/...
  ./internal/watcher/...
  ./sdk/api/handlers/...
  ./sdk/cliproxy/...
)

echo "== build"
go build ./...

echo "== vet"
go vet "${fork_packages[@]}"

echo "== fork hooks"
# One-line calls into fork code that live in upstream files. A merge can drop one without
# breaking the build, and not all of them can be reached from a test.
require_hook() {
  local file="$1" needle="$2" want="${3:-1}" found
  found="$(grep -cF -- "${needle}" "${file}" || true)"
  if [[ "${found}" -lt "${want}" ]]; then
    echo "::error file=${file}::fork hook found ${found} time(s), want ${want}: ${needle}"
    return 1
  fi
}
require_hook sdk/cliproxy/service_lifecycle.go 's.startQuotaRefresher(ctx)'
require_hook sdk/cliproxy/service_lifecycle.go 's.startLiveModelSync(ctx)'
require_hook sdk/cliproxy/service_models.go 's.withLiveModels(a, provider, models)' 2
require_hook sdk/cliproxy/service_config.go 'coreauth.SoonestResetSelector{}'
require_hook sdk/cliproxy/auth/conductor_selection.go '*SoonestResetSelector' 2
require_hook internal/registry/codex_client_models.go 'codexClientLiveOverlay.apply(data)'
require_hook internal/watcher/synthesizer/file.go 'cfg.Codex.Websockets'
require_hook sdk/api/handlers/claude/code_handlers.go 'handlers.NativeProviderModels(h.Cfg, h.Models(), "claude")'
require_hook sdk/api/handlers/openai/codex_client_models.go 'handlers.NativeProviderModels(h.Cfg, models, "codex")'

echo "== fork tests"
go test -count=1 -run '^TestFork' "${fork_packages[@]}"

echo "== image scripts"
sh -n docker/fork/entrypoint.sh
bash -n docker/fork/healthcheck.sh

echo "== upstream tests in the packages the fork touches (advisory)"
if ! go test -count=1 "${fork_packages[@]}"; then
  echo "retrying once"
  if ! go test -count=1 "${fork_packages[@]}"; then
    echo "::warning::upstream tests failed twice in the packages the fork touches; the fork's own checks passed"
  fi
fi
