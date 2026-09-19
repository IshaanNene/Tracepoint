#!/usr/bin/env bash
# Contract checks (§6.7). Run by `make contracts` and by CI.
#
#   1. The rename in ADR-010 stays complete: docs/SPEC.md is the only file allowed to
#      contain the spec's working name, because §0.1 requires it stored verbatim.
#   2. Every JSON Schema in schemas/ is a valid Draft 2020-12 schema.
#   3. Every fixture under schemas/testdata/ lands on the side its directory claims.
#
# Checks 2 and 3 need python3 with the `jsonschema` package. They are skipped with a
# warning when it is absent locally, and are hard requirements in CI (CI=true).
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
note() { printf '  %s\n' "$*"; }

echo "==> rename completeness (ADR-010)"
# The spec's working name is banned outright in code and contracts. Markdown may carry
# it only in the files that exist to explain the rename; docs/SPEC.md must, because
# §0.1 requires it stored verbatim.
prose_allowed="docs/SPEC.md docs/adr/010-name.md docs/ASSUMPTIONS.md docs/PROGRESS.md CLAUDE.md"
hits="$(grep -rIlniE '\bstrata' . \
  --exclude-dir=.git --exclude-dir=bin --exclude-dir=dist --exclude-dir=runs \
  --exclude-dir=node_modules --exclude=check-contracts.sh 2>/dev/null \
  | sed 's|^\./||' || true)"
bad=""
for f in $hits; do
  case "$f" in
    *.md) echo "$prose_allowed" | tr ' ' '\n' | grep -qxF "$f" || bad="$bad $f" ;;
    *)    bad="$bad $f" ;;
  esac
done
if [ -n "$bad" ]; then
  note "the old working name appears where it must not:"
  for f in $bad; do note "  $f"; done
  note "fix: use 'tracepoint' (see docs/adr/010-name.md). Only the listed docs may discuss the old name."
  fail=1
else
  note "ok — absent from all code and contracts; present only in the docs that explain the rename"
fi

echo "==> JSON Schemas"
if ! python3 -c 'import jsonschema' >/dev/null 2>&1; then
  if [ "${CI:-}" = "true" ]; then
    note "FAIL: python3 with the jsonschema package is required in CI"
    exit 1
  fi
  note "skipped — install with: python3 -m pip install jsonschema"
else
  python3 - <<'PY' || fail=1
import json, os, sys
from jsonschema import Draft202012Validator as V

ok = True
for fn in sorted(os.listdir('schemas')):
    if not fn.endswith('.json'):
        continue
    path = os.path.join('schemas', fn)
    try:
        V.check_schema(json.load(open(path)))
        print(f"  ok — {path}")
    except Exception as e:
        print(f"  FAIL — {path}: {e}")
        ok = False

root = 'schemas/testdata/config'
if os.path.isdir(root):
    v = V(json.load(open('schemas/config.schema.json')))
    counts = {}
    for sub, want_valid in (('valid', True), ('invalid', False)):
        d = os.path.join(root, sub)
        counts[sub] = 0
        for fn in sorted(os.listdir(d)):
            cfg = json.load(open(os.path.join(d, fn)))
            errs = list(v.iter_errors(cfg))
            got_valid = not errs
            counts[sub] += 1
            if got_valid != want_valid:
                why = errs[0].message[:100] if errs else 'accepted but should be rejected'
                print(f"  FAIL — {sub}/{fn}: {why}")
                ok = False
    print(f"  ok — fixtures: {counts.get('valid',0)} valid, {counts.get('invalid',0)} invalid, all classified correctly")

sys.exit(0 if ok else 1)
PY
fi

if [ "$fail" -ne 0 ]; then
  echo "contract checks FAILED"
  exit 1
fi
echo "contract checks passed"
