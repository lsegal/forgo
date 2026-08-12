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
# add cases for *ast.TryExpr, *ast.ThrowStmt, and *ast.PostfixIfStmt in
# both, and builds gopls against that patched copy via a go.mod replace
# directive. There may be other such switches elsewhere in x/tools not yet
# hit/patched; if forgopls dies with "internal error" or "unexpected
# node/statement type" pointing at one of these three from a different
# file, that file needs the same treatment — search x/tools for other
# exhaustive ast.Stmt/ast.Expr type switches with a default/panic case.
#
# It also vendors three forgo-specific quick-fix analyzers (source lives
# in forgopls/errtry, forgopls/comptimeconst, and forgopls/postfixif at
# the repo root, developed and tested standalone as their own Go module)
# into a similarly patched copy of the gopls module itself, and registers
# them in gopls' analyzer list — see the "forgo's own gopls analyzers"
# section below.
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
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

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
s = s.replace(
    ret,
    ret
    + "\n\tcase *ast.ThrowStmt:\n\t\t// forgo: throw X\n\t\twalk(v, edge.Invalid, -1, n.X)\n"
    + "\n\tcase *ast.PostfixIfStmt:\n\t\t// forgo: STMT if COND\n\t\twalk(v, edge.Invalid, -1, n.Stmt)\n\t\twalk(v, edge.Invalid, -1, n.Cond)\n",
    1,
)

open(path, "w").write(s)
PYEOF
	echo "patched $WALK_FILE for TryExpr/ThrowStmt/PostfixIfStmt"
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

# forgo: STMT if COND is conditional, like a plain "if COND { STMT }" with
# no else -- it has one nested statement (unlike throw/return, which are
# leaves) and doesn't always transfer control (unlike throw), so it needs
# its own cases in both passes rather than joining the leaf-statement list
# above. findLabels must still recurse into the wrapped statement to find
# any nested labels/breaks it contains.
findlabels_if = "\tcase *ast.IfStmt:\n\t\td.findLabels(x.Body)\n\t\tif x.Else != nil {\n\t\t\td.findLabels(x.Else)\n\t\t}\n"
assert findlabels_if in s, "findLabels IfStmt case not found; x/tools/go/analysis/passes/unreachable/unreachable.go layout may have changed"
s = s.replace(findlabels_if, findlabels_if + "\n\tcase *ast.PostfixIfStmt:\n\t\td.findLabels(x.Stmt)\n", 1)

dead = "\tcase *ast.ReturnStmt:\n\t\td.reachable = false\n"
assert dead in s, "findDead ReturnStmt case not found; x/tools/go/analysis/passes/unreachable/unreachable.go layout may have changed"
s = s.replace(dead, dead + "\n\tcase *ast.ThrowStmt:\n\t\t// forgo: throw X always transfers control, like return\n\t\td.reachable = false\n", 1)

# findDead's *ast.IfStmt case (with an Else) is a multi-line block, not a
# single fixed-text case like the others above, so match on its header line
# and insert PostfixIfStmt's own case (no-else "if" semantics: code after it
# is reachable regardless of whether the wrapped statement dead-ends) right
# before it instead of trying to reuse its body verbatim.
finddead_if_header = "\tcase *ast.IfStmt:\n\t\td.findDead(x.Body)\n"
assert finddead_if_header in s, "findDead IfStmt case not found; x/tools/go/analysis/passes/unreachable/unreachable.go layout may have changed"
s = s.replace(
    finddead_if_header,
    "\tcase *ast.PostfixIfStmt:\n\t\td.findDead(x.Stmt)\n\t\td.reachable = true // might not have executed\n\n"
    + finddead_if_header,
    1,
)

open(path, "w").write(s)
PYEOF
	echo "patched $UNREACHABLE_FILE for ThrowStmt/PostfixIfStmt"
fi

"$FORGO" mod edit -replace "golang.org/x/tools@${TOOLS_VERSION}=${PATCHED}"

# Register forgo's own gopls analyzers (see forgopls/errtry and
# forgopls/comptimeconst at the repo root, which are developed and tested
# standalone as their own module) as suggested-fix quick fixes: one for
# converting a manual "if err != nil { return zero..., err }" check to "?"
# (AGENTS.md rule 1), one for suggesting "//forgo:comptime" on a function
# used to compute a package-level "var" from constant arguments (AGENTS.md
# rule 2). This vendors their source into a patched copy of the gopls
# module (gopls is itself a nested module inside x/tools, so it needs the
# same copy-patch-replace treatment as x/tools above) and wires them into
# gopls' analyzer registry.
GOPLS_SRC="$("$FORGO" list -m -f '{{.Dir}}' golang.org/x/tools/gopls)"
PATCHED_GOPLS="$WORK/gopls-patched"
cp -R "$GOPLS_SRC" "$PATCHED_GOPLS"
chmod -R u+w "$PATCHED_GOPLS"

mkdir -p "$PATCHED_GOPLS/internal/analysis/errtry" "$PATCHED_GOPLS/internal/analysis/comptimeconst" "$PATCHED_GOPLS/internal/analysis/postfixif"
cp "$REPO_ROOT/forgopls/errtry/errtry.go" "$PATCHED_GOPLS/internal/analysis/errtry/"
cp "$REPO_ROOT/forgopls/comptimeconst/comptimeconst.go" "$PATCHED_GOPLS/internal/analysis/comptimeconst/"
cp "$REPO_ROOT/forgopls/postfixif/postfixif.go" "$PATCHED_GOPLS/internal/analysis/postfixif/"

ANALYSIS_FILE="$PATCHED_GOPLS/internal/settings/analysis.go"
if ! grep -q 'analysis/errtry' "$ANALYSIS_FILE"; then
	"$PYTHON" - "$ANALYSIS_FILE" <<'PYEOF'
import sys
path = sys.argv[1]
s = open(path).read()

deprecated_import = '\t"golang.org/x/tools/gopls/internal/analysis/deprecated"\n'
assert deprecated_import in s, "deprecated import not found; gopls/internal/settings/analysis.go layout may have changed"
s = s.replace(deprecated_import, '\t"golang.org/x/tools/gopls/internal/analysis/comptimeconst"\n' + deprecated_import, 1)

fillreturns_import = '\t"golang.org/x/tools/gopls/internal/analysis/fillreturns"\n'
assert fillreturns_import in s, "fillreturns import not found; gopls/internal/settings/analysis.go layout may have changed"
s = s.replace(fillreturns_import, '\t"golang.org/x/tools/gopls/internal/analysis/errtry"\n' + fillreturns_import, 1)

nonewvars_import = '\t"golang.org/x/tools/gopls/internal/analysis/nonewvars"\n'
assert nonewvars_import in s, "nonewvars import not found; gopls/internal/settings/analysis.go layout may have changed"
s = s.replace(nonewvars_import, '\t"golang.org/x/tools/gopls/internal/analysis/postfixif"\n' + nonewvars_import, 1)

unusedwrite = '{analyzer: unusedwrite.Analyzer, severity: protocol.SeverityInformation}, // uses go/ssa\n'
assert unusedwrite in s, "unusedwrite entry not found; gopls/internal/settings/analysis.go layout may have changed"
forgo_entries = (
	'\n\t\t// forgo-specific suggestions (not part of upstream gopls; see AGENTS.md)\n'
	'\t\t{analyzer: errtry.Analyzer, severity: protocol.SeverityHint},\n'
	'\t\t{analyzer: comptimeconst.Analyzer, severity: protocol.SeverityHint},\n'
	'\t\t{analyzer: postfixif.Analyzer, severity: protocol.SeverityHint},\n'
)
s = s.replace(unusedwrite, unusedwrite + forgo_entries, 1)

open(path, "w").write(s)
PYEOF
	echo "patched $ANALYSIS_FILE to register errtry/comptimeconst/postfixif"
fi

"$FORGO" mod edit -replace "golang.org/x/tools/gopls@${GOPLS_VERSION}=${PATCHED_GOPLS}"

mkdir -p "$(dirname "$OUT")"
GOBIN="$WORK/bin" "$FORGO" install golang.org/x/tools/gopls
mv "$WORK/bin/gopls" "$OUT"
echo "built $OUT"
