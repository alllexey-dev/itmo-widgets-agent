#!/bin/sh
# Runs install.sh against a fake release: scripts/install-test.sh INSTALLER VERSION.
# IW_RELEASE_BASE serves VERSION/<assets> with a good SHA256SUMS and bad/<assets> with a wrong one.
# Needs no network beyond that server: pairing is skipped through a token fixture, the service through --no-service.
set -eu
installer=$1 v=$2
HOME=$(mktemp -d)
export HOME IW_STATE_DIR="$HOME/state" IW_BACKEND_URL=http://127.0.0.1:9
bin="$HOME/.local/bin/itmo-widgets-agent"
fail() { echo "FAIL: $*" >&2; exit 1; }
mkdir -p "$IW_STATE_DIR"
echo iwa_test_fixture >"$IW_STATE_DIR/agent-token"

out=$(sh "$installer" --version "$v" --no-service) || fail "install: $out"
echo "$out"
[ "$("$bin" version)" = "${v#v}" ] || fail "installed version"
echo "$out" | grep -q 'pair пропущен' || fail "pairing not skipped with a token"

out=$(sh "$installer" --status --version "$v") || fail "status: $out"
echo "$out"
echo "$out" | grep -q "Агент:   ${v#v} (последняя)" || fail "status: version"
echo "$out" | grep -q 'Телефон: подключён' || fail "status: paired"
echo "$out" | grep -q 'Служба:  не установлена' || fail "status: service"

sh "$installer" --version "$v" --no-service >/dev/null || fail "re-run"
[ -x "$bin" ] || fail "re-run lost the binary"

cp "$bin" "$HOME/before"
if sh "$installer" --version bad --no-service; then fail "a wrong checksum installed"; fi
cmp -s "$bin" "$HOME/before" || fail "a wrong checksum replaced the binary"

if sh "$installer" --unpair </dev/null; then fail "unpair without --yes and a terminal"; fi
[ -f "$IW_STATE_DIR/agent-token" ] || fail "unpair without --yes removed the token"

sh "$installer" --uninstall --yes || fail "uninstall"
[ ! -e "$bin" ] || fail "uninstall left the binary"
[ ! -e "$IW_STATE_DIR/agent-token" ] || fail "uninstall left the token"
echo "install.sh: all checks passed"
