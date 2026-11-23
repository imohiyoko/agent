#!/usr/bin/env bash
# リポジトリの検査。CI と同じものを手元で流す: bash scripts/check.sh
# 1 つでも失敗したら止まらずに最後まで流し、最後に失敗の一覧を出す。
set -uo pipefail
cd "$(dirname "$0")/.."

fails=()
step() { echo "== $1"; }
fail() { echo "FAIL: $1"; fails+=("$1"); }

step "gofmt"
out=$(gofmt -l . 2>&1)
[ -z "$out" ] || fail "gofmt: $out"

step "go vet (linux, darwin)"
GOOS=linux go vet ./... || fail "go vet linux"
GOOS=darwin GOARCH=arm64 go vet ./... || fail "go vet darwin"

step "staticcheck"
if command -v staticcheck >/dev/null; then
  staticcheck ./... || fail "staticcheck"
else
  echo "skip: staticcheck not installed (CI runs it)"
fi

step "go test"
go test -race -count=1 ./... || fail "go test"

step "build (linux, darwin)"
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  GOOS=${t%/*} GOARCH=${t#*/} go build -o /dev/null ./cmd/nsctl || fail "build $t"
done

step "examples validate"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
cp -r examples/namespaces "$tmp/"
printf 'namespaces_dir: namespaces\nstate_dir: %s/state\n' "$tmp" > "$tmp/nsctl.yaml"
go run ./cmd/nsctl validate -config "$tmp/nsctl.yaml" || fail "examples validate"

step "secrets in tracked files"
# 本物らしい秘密の形を探す。テストの固定値は secret-a のような明らかな偽物にする。
pat='(nsk_[A-Za-z0-9_-]{40,}|ghp_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-ant-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|xox[baprs]-[A-Za-z0-9-]{10,})'
if git ls-files -z --cached --others --exclude-standard | xargs -0 grep -nIE "$pat" -- 2>/dev/null | grep -v '^scripts/check.sh:'; then
  fail "possible secret in tracked files"
fi

step "forbidden files"
if git ls-files --cached --others --exclude-standard | grep -E '(^|/)(\.env(\..*)?|secrets\.ya?ml|tokens\.json|id_(rsa|ed25519))$'; then
  fail "a secret-holding file is tracked"
fi

step "design invariant 5: OpenShell only in internal/openshell"
if grep -rniI --include='*.go' 'openshell' . | grep -v '^./internal/openshell/' | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//'; then
  fail "OpenShell referenced outside internal/openshell"
fi

step "no unsandboxed fallback"
# サンドボックスが使えないときに制限なしで動かす分岐を作らない(ADR 0002)。
if grep -rnI --include='*.go' -E 'exec\.Command(Context)?\(' internal cmd | grep -vE '_test\.go:|internal/sandbox/|// sandbox-ok'; then
  fail "exec.Command outside internal/sandbox (wrap it with sandbox.Command, or mark a reviewed line with // sandbox-ok)"
fi

step "ADR index"
for f in docs/adr/[0-9][0-9][0-9][0-9]-*.md; do
  b=$(basename "$f")
  grep -q "($b)" docs/adr/README.md || fail "ADR $b is not listed in docs/adr/README.md"
done

step "skills have frontmatter"
for f in .claude/skills/*/SKILL.md; do
  [ -e "$f" ] || continue
  head -1 "$f" | grep -q '^---$' && grep -q '^name: ' "$f" && grep -q '^description: ' "$f" || fail "skill $f lacks name/description frontmatter"
done

echo
if [ ${#fails[@]} -gt 0 ]; then
  echo "${#fails[@]} check(s) failed:"; printf ' - %s\n' "${fails[@]}"; exit 1
fi
echo "all checks passed"
