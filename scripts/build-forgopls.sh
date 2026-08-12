#!/usr/bin/env bash
# Builds forgopls: gopls (golang.org/x/tools/gopls), compiled with the forgo
# toolchain so it links against this repo's patched go/parser, go/ast, and
# go/types (see src/go/types/expr.go and friends) instead of vanilla Go's —
# enough to parse and type-check ? and throw instead of erroring on them.
#
# That alone isn't sufficient: gopls also depends on x/tools packages that
# do their own hand-written, exhaustive switches over every known ast.Node
# type (distinct from go/ast.Walk, which this repo's go/ast/walk.go already
# handles) — go/ast/inspector (a fast AST walker gopls uses pervasively) and
# go/analysis/passes/unreachable (a vet-style check) are the two known so
# far; both hard-fail (panic, or log.Fatal — an outright process exit) on
# an unrecognized node type. This script patches a local copy of x/tools to
# add cases for *ast.TryExpr and *ast.ThrowStmt in both, and builds gopls
# against that patched copy via a go.mod replace directive. There may be
# other such switches elsewhere in x/tools not yet hit/patched; if forgopls
# dies with "internal error" or "unexpected node/statement type" pointing at
# TryExpr/ThrowStmt from a different file, that file needs the same
# treatment — search x/tools for other exhaustive ast.Stmt/ast.Expr type
# switches with a default/panic case.
#
# Usage:
#   GOROOT=/path/to/forgo GOTOOLCHAIN=local scripts/build-forgopls.sh [gopls version] [output path]
#
# Requires GOROOT to already point at a built forgo toolchain (bin/forgo).
set -euo pipefail

GOPLS_VERSION="${1:-v0.23.0}"
OUT="${2:-$GOROOT/bin/forgopls}"

if [ -z "${GOROOT:-}" ] || [ ! -x "$GOROOT/bin/forgo" ]; then
	echo "error: GOROOT must point at a built forgo toolchain (missing \$GOROOT/bin/forgo)" >&2
	exit 1
fi
export GOTOOLCHAIN=local
FORGO="$GOROOT/bin/forgo"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"

"$FORGO" mod init forgopls-build >/dev/null
"$FORGO" get "golang.org/x/tools/gopls@${GOPLS_VERSION}" >/dev/null

TOOLS_VERSION="$("$FORGO" list -m -f '{{.Version}}' golang.org/x/tools)"
TOOLS_SRC="$("$FORGO" list -m -f '{{.Dir}}' golang.org/x/tools)"
PATCHED="$WORK/x-tools-patched"
cp -R "$TOOLS_SRC" "$PATCHED"
chmod -R u+w "$PATCHED"

PYTHON="$(command -v python3 || command -v python)"

WALK_FILE="$PATCHED/go/ast/inspector/walk.go"
if ! grep -q 'ast.TryExpr' "$WALK_FILE"; then
	"$PYTHON" - "$WALK_FILE" <<'PYEOF'
import sys
path = sys.argv[1]
s = open(path).read()

kv = "\tcase *ast.KeyValueExpr:\n\t\twalk(v, edge.KeyValueExpr_Key, -1, n.Key)\n\t\twalk(v, edge.KeyValueExpr_Value, -1, n.Value)\n"
assert kv in s, "KeyValueExpr case not found; x/tools/go/ast/inspector/walk.go layout may have changed"
s = s.replace(kv, kv + "\n\tcase *ast.TryExpr:\n\t\t// forgo: X?\n\t\twalk(v, edge.Invalid, -1, n.X)\n", 1)

ret = "\tcase *ast.ReturnStmt:\n\t\twalkList(v, edge.ReturnStmt_Results, n.Results)\n"
assert ret in s, "ReturnStmt case not found; x/tools/go/ast/inspector/walk.go layout may have changed"
s = s.replace(ret, ret + "\n\tcase *ast.ThrowStmt:\n\t\t// forgo: throw X\n\t\twalk(v, edge.Invalid, -1, n.X)\n", 1)

open(path, "w").write(s)
PYEOF
	echo "patched $WALK_FILE for TryExpr/ThrowStmt"
fi

UNREACHABLE_FILE="$PATCHED/go/analysis/passes/unreachable/unreachable.go"
if ! grep -q 'ast.ThrowStmt' "$UNREACHABLE_FILE"; then
	"$PYTHON" - "$UNREACHABLE_FILE" <<'PYEOF'
import sys
path = sys.argv[1]
s = open(path).read()

labels = "\tcase *ast.AssignStmt,\n\t\t*ast.BadStmt,\n\t\t*ast.DeclStmt,\n\t\t*ast.DeferStmt,\n\t\t*ast.EmptyStmt,\n\t\t*ast.ExprStmt,\n\t\t*ast.GoStmt,\n\t\t*ast.IncDecStmt,\n\t\t*ast.ReturnStmt,\n\t\t*ast.SendStmt:\n\t\t// no statements inside\n"
assert labels in s, "findLabels leaf-statement case not found; x/tools/go/analysis/passes/unreachable/unreachable.go layout may have changed"
s = s.replace(
    labels,
    labels.replace("*ast.SendStmt:", "*ast.SendStmt,\n\t\t*ast.ThrowStmt:"),  # forgo: throw X has no statements inside, like return
    1,
)

dead = "\tcase *ast.ReturnStmt:\n\t\td.reachable = false\n"
assert dead in s, "findDead ReturnStmt case not found; x/tools/go/analysis/passes/unreachable/unreachable.go layout may have changed"
s = s.replace(dead, dead + "\n\tcase *ast.ThrowStmt:\n\t\t// forgo: throw X always transfers control, like return\n\t\td.reachable = false\n", 1)

open(path, "w").write(s)
PYEOF
	echo "patched $UNREACHABLE_FILE for ThrowStmt"
fi

"$FORGO" mod edit -replace "golang.org/x/tools@${TOOLS_VERSION}=${PATCHED}"

mkdir -p "$(dirname "$OUT")"
GOBIN="$WORK/bin" "$FORGO" install golang.org/x/tools/gopls
mv "$WORK/bin/gopls" "$OUT"
echo "built $OUT"
