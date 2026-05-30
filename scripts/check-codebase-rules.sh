#!/usr/bin/env bash
# Enforce the load-bearing canonical-protocol codebase rules via grep.
# Source of truth: docs/canonical-protocol.md "Codebase rules".
# CLAUDE.md: "The grep tests for rules 1, 2, 4, 10 must hold on every commit."
#
# These are import-graph checks — the unambiguous, automatable core of the
# rules. (Rule 4's broader "no vendor *names* in app/" also covers error
# strings / catalog data, which is a human-review grep, not pass/fail; here
# we enforce the clean part: app/ never imports a concrete vendor adapter.)
set -uo pipefail
cd "$(dirname "$0")/.."

fail=0
note() { printf '  \033[31m✗\033[0m %s\n' "$1"; fail=1; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }

vendors="openai anthropic gemini"

echo "Rule 1 & 10 — SDK purity (sdk/ imports nothing from app/ or internal/):"
hits=$(grep -rnE 'wyolet/relay/(app|internal)' sdk/ --include='*.go' || true)
if [ -n "$hits" ]; then note "sdk/ imports server-only code:"; echo "$hits"; else ok "clean"; fi

echo "Rule 2 — vendor adapters never import each other:"
r2=0
for v in $vendors; do
  others=$(echo "$vendors" | tr ' ' '\n' | grep -v "^$v$" | paste -sd'|' -)
  hits=$(grep -rnE "wyolet/relay/sdk/adapters/($others)" "sdk/adapters/$v/" --include='*.go' | grep -v '_test.go' || true)
  if [ -n "$hits" ]; then note "$v imports another vendor:"; echo "$hits"; r2=1; fi
done
[ $r2 -eq 0 ] && ok "clean"

echo "Rule 4 — app/ never imports a concrete vendor adapter (composition root only):"
# Tests legitimately import a concrete adapter (e.g. gemini_integration_test);
# the rule governs non-test app/ code.
hits=$(grep -rnE 'wyolet/relay/sdk/adapters/(openai|anthropic|gemini)' app/ --include='*.go' | grep -v '_test.go' || true)
if [ -n "$hits" ]; then note "app/ imports a vendor adapter (should go through the Registry):"; echo "$hits"; else ok "clean"; fi

if [ $fail -ne 0 ]; then
  echo
  echo "Codebase-rule check FAILED — see docs/canonical-protocol.md \"Codebase rules\"."
  exit 1
fi
echo
echo "All codebase-rule checks passed."
