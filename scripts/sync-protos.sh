#!/usr/bin/env bash
#
# Propagate the .proto contracts to every repository that vendors them.
#
#   bash scripts/sync-protos.sh            # copy source-of-truth → vendored copies
#   bash scripts/sync-protos.sh --check    # verify only; exit 1 on any drift (CI)
#
# ─── Why this exists ─────────────────────────────────────────────────────────
#
# operator.proto lives in four places: cambrian-core (the source of truth), ui/,
# cli/, and — as generated Python stubs rather than .proto — cambrian-benchmarks.
# Keeping them aligned was a manual step with nothing checking it, and it drifted:
# core gained the whole GetDocument RPC (the ADR-0095 keyed read) and neither ui/
# nor cli/ received it, so the console could not resolve a document id to its body
# and nothing reported that as a problem.
#
# ─── Line endings ────────────────────────────────────────────────────────────
#
# Comparison strips CR. cambrian-core was historically checked out CRLF while ui/
# and cli/ were LF, so a byte comparison called every file totally different — the
# real drift was 34 lines and `diff` reported 4,788. That noise is what let the
# drift survive. See .gitattributes for the durable fix.
#
# ─── Scope ───────────────────────────────────────────────────────────────────
#
# .proto SOURCES only. cambrian-benchmarks vendors GENERATED stubs
# (src/cambrian_bench/transport/_stubs/*_pb2*.py); regenerating those needs the
# Python toolchain, so this script reports on them rather than rewriting them.
#
set -uo pipefail

core="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
root="$(cd "$core/.." && pwd)"
premium="$root/cambrian-premium"

check_only=0
[ "${1:-}" = "--check" ] && check_only=1

# "source|destination" — source is always the module that OWNS the contract.
mappings=(
    "$core/api/proto/operator.proto|$root/ui/proto/operator.proto"
    "$core/api/proto/operator.proto|$root/cli/proto/operator.proto"
    "$core/api/proto/cambrian.proto|$root/cli/proto/cambrian.proto"
    "$premium/api/proto/authz/access_policy.proto|$root/ui/proto/authz/access_policy.proto"
    "$premium/api/proto/records/records.proto|$root/ui/proto/records/records.proto"
    "$premium/api/proto/telegram/telegram_admin.proto|$root/ui/proto/telegram/telegram_admin.proto"
    # ADR-0112. This copy was HAND-MAINTAINED and had already drifted: the UI's
    # Rust client compiles from ui/proto, so a premium-side RPC simply did not
    # exist for the console until somebody remembered to copy the file. That is
    # the exact failure this script exists to prevent, so it is listed here now.
    "$premium/api/proto/ingress/ingress_studio.proto|$root/ui/proto/ingress/ingress_studio.proto"
    "$premium/api/proto/telemetry/telemetry_admin.proto|$root/ui/proto/telemetry/telemetry_admin.proto"
    # The substrate plane (ADR-0108/0111/0118). Vendored in ui/ since it was
    # written and listed here only now, which is the same omission the ingress
    # entry above records: the copy existed, nothing checked it, and the moment
    # five-planes step 2 added `entity`/`why`/`expand_aliases` to the source the
    # two files were free to disagree with nobody noticing.
    "$premium/api/proto/substrate/substrate.proto|$root/ui/proto/substrate/substrate.proto"
    # Not a proto, same problem. cambrian-benchmarks re-implements the Go chunkers
    # in Python; this fixture is what its differential test asserts the ports
    # against. Regenerate on the Go side with:
    #   go test ./internal/memory/ -run TestChunkerGolden -update-golden
    "$core/internal/memory/testdata/chunker_golden.json|$root/cambrian-benchmarks/src/cambrian_bench/suites/chunking/testdata/chunker_golden.json"
)

drift=0
synced=0
skipped=0

for m in "${mappings[@]}"; do
    src="${m%%|*}"
    dst="${m##*|}"
    rel_dst="${dst#"$root"/}"

    if [ ! -f "$src" ]; then
        echo "SKIP  $rel_dst (source missing: ${src#"$root"/})"
        skipped=$((skipped + 1))
        continue
    fi
    if [ ! -f "$dst" ]; then
        # A missing destination is a real finding in --check: the consumer repo is
        # not vendoring a contract it is expected to. It is not an error to fix by
        # creating the file blindly, because the consumer may legitimately not use it.
        echo "MISS  $rel_dst (not vendored)"
        skipped=$((skipped + 1))
        continue
    fi

    if diff -q --strip-trailing-cr "$src" "$dst" >/dev/null 2>&1; then
        continue
    fi

    n=$(diff --strip-trailing-cr "$src" "$dst" | grep -c '^[<>]' || true)
    if [ "$check_only" -eq 1 ]; then
        echo "DRIFT $rel_dst ($n differing lines vs ${src#"$root"/})"
        drift=$((drift + 1))
    else
        cp "$src" "$dst"
        echo "SYNC  $rel_dst ($n lines updated)"
        synced=$((synced + 1))
    fi
done

# Report-only: the Python harness vendors generated stubs, not .proto sources.
stubs="$root/cambrian-benchmarks/src/cambrian_bench/transport/_stubs"
if [ -d "$stubs" ]; then
    echo "NOTE  cambrian-benchmarks vendors GENERATED stubs in ${stubs#"$root"/};"
    echo "      regenerate with the Python toolchain after a contract change."
fi

if [ "$check_only" -eq 1 ]; then
    if [ "$drift" -ne 0 ]; then
        echo
        echo "FAIL: $drift vendored proto copy/copies are out of date."
        echo "      Run: bash scripts/sync-protos.sh   (then commit in each repo)"
        exit 1
    fi
    echo "PASS: every vendored .proto matches its source of truth."
    exit 0
fi

echo
echo "Done: $synced synced, $skipped skipped."
[ "$synced" -gt 0 ] && echo "Commit the updated files in each consumer repository."
exit 0
