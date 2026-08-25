#!/usr/bin/env bash
set -euo pipefail

repository_url='https://github.com/ly95/agentruntime'
security_url="${repository_url}/security/advisories/new"

source_version=$(sed -n 's/^const Version = "\([^"]*\)"/\1/p' version.go)
test -n "$source_version"
test -f "api/v${source_version}.txt"
test -f "docs/releases/v${source_version}.md"
grep -Fq "## [${source_version}]" CHANGELOG.md
grep -Fq "[v${source_version} release notes](docs/releases/v${source_version}.md)" README.md

if git grep -n -E 'github\.com/ly95/agent-go|label: .*agent-go' -- . ':!.codegraph' ':!.codex-tmp' ':!scripts/check-repository-contracts.sh'; then
  echo 'repository contract check: stale agent-go repository identity found' >&2
  exit 1
fi

git grep -Fq "url: ${security_url}" -- .github/ISSUE_TEMPLATE/config.yml
git grep -Fq "${security_url}" -- SECURITY.md

if [[ "${CHECK_REMOTE_LINKS:-0}" == '1' ]]; then
  curl --fail --silent --show-error --location --retry 3 --retry-all-errors --max-time 20 \
    --output /dev/null "${security_url}"
fi
