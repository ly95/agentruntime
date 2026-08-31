#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
readonly script_dir
repo_root=$(cd -- "$script_dir/.." && pwd -P)
readonly repo_root
readonly gate="$script_dir/check-api-baseline.sh"

temporary_dir=$(mktemp -d)
readonly temporary_dir
readonly invocation_log="$temporary_dir/go-invocation.log"
readonly command_stdout="$temporary_dir/command.stdout"
readonly command_stderr="$temporary_dir/command.stderr"
trap 'rm -f "$invocation_log" "$command_stdout" "$command_stderr"; rmdir "$temporary_dir"' EXIT

go() {
  {
    printf 'pwd=%s\n' "$PWD"
    printf 'toolchain=%s\n' "${GOTOOLCHAIN:-}"
    printf 'arg=%s\n' "$@"
  } >"$API_GATE_TEST_LOG"
  if [[ "${API_GATE_TEST_FAIL:-0}" == '1' ]]; then
    echo 'simulated apidiff failure' >&2
    return 7
  fi
  if [[ "${API_GATE_TEST_DIFF:-0}" == '1' ]]; then
    printf '%s\n' 'Compatible changes:' '- ReviewOnlyAPI: added'
  fi
}
export -f go

assert_invocation() {
  local expected_baseline=$1
  grep -Fxq "pwd=$repo_root" "$invocation_log"
  grep -Fxq 'toolchain=go1.26.0' "$invocation_log"
  grep -Fxq 'arg=run' "$invocation_log"
  grep -Fxq 'arg=golang.org/x/exp/cmd/apidiff@v0.0.0-20260824195058-e88cd73687aa' "$invocation_log"
  grep -Fxq 'arg=-m' "$invocation_log"
  grep -Fxq "arg=$expected_baseline" "$invocation_log"
  grep -Fxq 'arg=github.com/ly95/agentruntime' "$invocation_log"
}

(
  cd -- "$temporary_dir"
  API_GATE_TEST_LOG="$invocation_log" bash "$gate"
)
assert_invocation "$repo_root/api/main.txt"

(
  cd -- "$temporary_dir"
  API_BASELINE='api/v0.1.0.txt' API_GATE_TEST_LOG="$invocation_log" bash "$gate"
)
assert_invocation "$repo_root/api/v0.1.0.txt"

(
  cd -- "$temporary_dir"
  API_BASELINE="$repo_root/api/v0.1.0.txt" API_GATE_TEST_LOG="$invocation_log" bash "$gate"
)
assert_invocation "$repo_root/api/v0.1.0.txt"

set +e
API_BASELINE='api/v0.1.0.txt' API_GATE_TEST_LOG="$invocation_log" bash "$gate" --write >"$command_stdout" 2>"$command_stderr"
rewrite_status=$?
set -e
if [[ "$rewrite_status" -ne 2 ]]; then
  echo "tagged API baseline rewrite returned $rewrite_status instead of 2" >&2
  exit 1
fi
grep -Fq 'refusing to rewrite a non-main API baseline' "$command_stderr"

set +e
(
  cd -- "$temporary_dir"
  API_BASELINE='api/missing.txt' API_GATE_TEST_LOG="$invocation_log" bash "$gate"
) >"$command_stdout" 2>"$command_stderr"
missing_status=$?
set -e
if [[ "$missing_status" -ne 1 ]]; then
  echo "missing API baseline returned $missing_status instead of 1" >&2
  exit 1
fi
grep -Fq "public API baseline not found: $repo_root/api/missing.txt" "$command_stderr"

(
  cd -- "$temporary_dir"
  API_BASELINE="$repo_root/api/main.txt" API_GATE_TEST_LOG="$invocation_log" bash "$gate" --write
)
assert_invocation "$repo_root/api/main.txt"
grep -Fxq 'arg=-w' "$invocation_log"

if (
  cd -- "$temporary_dir"
  API_GATE_TEST_DIFF=1 API_GATE_TEST_LOG="$invocation_log" bash "$gate"
) >"$command_stdout" 2>"$command_stderr"; then
  echo 'API difference unexpectedly passed' >&2
  exit 1
fi
grep -Fq 'public API differs from the reviewed baseline' "$command_stderr"
grep -Fq 'ReviewOnlyAPI: added' "$command_stderr"

set +e
(
  cd -- "$temporary_dir"
  API_GATE_TEST_FAIL=1 API_GATE_TEST_LOG="$invocation_log" bash "$gate"
) >"$command_stdout" 2>"$command_stderr"
failure_status=$?
set -e
if [[ "$failure_status" -ne 7 ]]; then
  echo "apidiff failure status changed from 7 to $failure_status" >&2
  exit 1
fi
grep -Fq 'simulated apidiff failure' "$command_stderr"
