#!/usr/bin/env bash
set -euo pipefail

readonly apidiff_version='v0.0.0-20260824195058-e88cd73687aa'
readonly go_toolchain='go1.26.0'
readonly module_path='github.com/ly95/agentruntime'

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
readonly script_dir
repo_root=$(cd -- "$script_dir/.." && pwd -P)
readonly repo_root
readonly baseline_input="${API_BASELINE:-api/main.txt}"
if [[ "$baseline_input" == /* ]]; then
  baseline="$baseline_input"
else
  baseline="$repo_root/$baseline_input"
fi
readonly baseline
readonly main_baseline="$repo_root/api/main.txt"

canonical_target() {
  local target=$1
  local parent
  parent=$(cd -- "$(dirname -- "$target")" 2>/dev/null && pwd -P) || return 1
  printf '%s/%s\n' "$parent" "$(basename -- "$target")"
}

cd -- "$repo_root"

case "${1:-}" in
  --write)
    canonical_baseline=$(canonical_target "$baseline") || true
    canonical_main=$(canonical_target "$main_baseline") || true
    if [[ -z "$canonical_baseline" || "$canonical_baseline" != "$canonical_main" || -L "$baseline" ]]; then
      echo 'refusing to rewrite a non-main API baseline' >&2
      exit 2
    fi
    GOTOOLCHAIN="$go_toolchain" go run "golang.org/x/exp/cmd/apidiff@${apidiff_version}" \
      -m -w "$baseline" "$module_path"
    exit 0
    ;;
  '')
    ;;
  *)
    echo 'usage: scripts/check-api-baseline.sh [--write]' >&2
    exit 2
    ;;
esac

if [[ ! -f "$baseline" ]]; then
  echo "public API baseline not found: $baseline" >&2
  exit 1
fi

diff_file=$(mktemp)
trap 'rm -f "$diff_file"' EXIT

GOTOOLCHAIN="$go_toolchain" go run "golang.org/x/exp/cmd/apidiff@${apidiff_version}" \
  -m "$baseline" "$module_path" >"$diff_file"

if [[ -s "$diff_file" ]]; then
  echo "public API differs from the reviewed baseline $baseline:" >&2
  cat "$diff_file" >&2
  echo 'Update the selected snapshot only during reviewed main or release preparation; never rewrite a published release snapshot.' >&2
  exit 1
fi
