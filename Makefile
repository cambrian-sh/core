# Cambrian Runtime — Test & Benchmark Runner
# Usage: make <target>
# All targets assume `go`, `benchstat`, `pytest`, and `docker` are in PATH.

.PHONY: help \
        test integration chaos chaos-real \
        leak leak-integration \
        bench-micro bench-macro bench-compare \
        fuzz fuzz-release \
        contract contract-release \
        separability \
        lint lint-new deadcode \
        proto-sync proto-sync-check \
        corpus export \
        per-pr nightly release-gate

# ─── Default ─────────────────────────────────────────────────────────────────

help:
	@echo ""
	@echo "Cambrian Runtime — Test & Benchmark Runner"
	@echo ""
	@echo "  FAST (no external deps)"
	@echo "    make test              Unit tests, all packages                  ~30s"
	@echo "    make separability      OTel / premium import gate                 ~1s"
	@echo "    make lint              golangci-lint, full report (advisory)      ~40s"
	@echo "    make lint-new          golangci-lint vs $(LINT_BASE) (the gate)     ~40s"
	@echo "    make deadcode          Unreachable funcs across all binaries      ~20s"
	@echo "    make integration       SystemHarness E2E tests                    ~5s"
	@echo "    make chaos             Per-PR chaos scenarios (6, in-process)     <5s"
	@echo "    make leak              Package-level goroutine leak detection      ~3s"
	@echo "    make bench-micro       Micro benchmarks vs git baseline           ~10s"
	@echo ""
	@echo "  SLOW (dedicated runner or Docker required)"
	@echo "    make bench-macro       Macro benchmarks (nightly runner)          ~6m"
	@echo "    make bench-compare     Micro benchmarks + benchstat diff          ~15s"
	@echo "    make chaos-real        Real-service chaos (Docker required)       8-20m"
	@echo "    make leak-integration  Full-kernel goroutine leak test            ~30s"
	@echo "    make fuzz              Fuzzing, 10 minutes                        10m"
	@echo "    make fuzz-release      Fuzzing, 1 hour (release gate)             1h"
	@echo "    make contract          Agent mock contract tests (Python)         ~10s"
	@echo "    make contract-release  Agent real-Substrate contract tests        ~2m"
	@echo ""
	@echo "  DATA"
	@echo "    make corpus            Generate 1000-record synthetic corpus       <1s"
	@echo "    make export            Export real bbolt events to JSONL           ~2s"
	@echo ""
	@echo "  PIPELINES"
	@echo "    make per-pr            Full per-PR pipeline (no Docker)            ~2m"
	@echo "    make nightly           Nightly pipeline (dedicated runner)         ~15m"
	@echo "    make release-gate      Full release gate (Docker + 1h fuzz)        ~30m"
	@echo ""

# ─── Unit Tests ──────────────────────────────────────────────────────────────

test:
	go test ./internal/...

# QA-01: the race gate. In `per-pr` rather than `nightly` because the cost was
# MEASURED, not guessed: 16s plain vs 41s with -race across ./internal/... — a
# 25-second tax on a pipeline that already runs integration, chaos and micro
# benchmarks. Nightly-only was the fallback if that had been unacceptable; it was
# not, and a race found tomorrow morning is a race that already merged.
test-race:
	go test -race ./internal/...

# ─── Lint & Dead Code ────────────────────────────────────────────────────────

GOLANGCI ?= golangci-lint
DEADCODE ?= deadcode

# Full report. ADVISORY, not a gate: the tree carried ~131 pre-existing findings
# when the config landed, and blocking every PR on a backlog nobody has triaged is
# how a linter gets disabled. Use `lint-new` for the gate.
lint:
	@command -v $(GOLANGCI) >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed."; \
		echo "    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; \
	}
	$(GOLANGCI) run ./...

# The GATE: only issues introduced by this branch fail. This is what makes the
# linter adoptable — existing findings are paid down deliberately while new code
# is held to the full standard from day one.
#
# Needs full history and the base ref present; a shallow clone reports everything
# as new. LINT_BASE is overridable for a branch cut from something other than main.
LINT_BASE ?= origin/main
lint-new:
	@command -v $(GOLANGCI) >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed."; \
		echo "    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; \
	}
	$(GOLANGCI) run --new-from-rev=$(LINT_BASE) ./...

# Unreachable exported+unexported functions across ALL binaries.
#
# `./cmd/...`, never a single binary: analysing only cmd/orchestrator wrongly
# condemns internal/memory/pagerank.go and internal/metabolism/routescorer/,
# which other maintenance binaries use. It also cannot see cross-MODULE callers,
# so app/plugin.go's Registry.* (the premium plugin API) always appears
# unreachable — grep cambrian-premium before deleting anything there.
deadcode:
	@command -v $(DEADCODE) >/dev/null 2>&1 || { \
		echo "deadcode is not installed."; \
		echo "    go install golang.org/x/tools/cmd/deadcode@latest"; \
		exit 1; \
	}
	$(DEADCODE) ./cmd/...

# ─── Separability Gate ───────────────────────────────────────────────────────

# The check lives in scripts/check-separability.sh — a real script with a real
# exit code, not an inline grep. The previous inline fallback scanned
# `internal/domain` (which does not exist; domain/ is at the repo root) and
# grepped for `internal/premium` (a path retired by ADR-0057), so it passed
# vacuously and could not have caught a boundary violation.
#
# bash is preferred because it is what CI runs; the pwsh variant is kept at
# parity for Windows developers without a bash on PATH.
separability:
	@echo "--- Checking OTel / premium separability ---"
	@if command -v bash > /dev/null 2>&1; then \
		bash scripts/check-separability.sh; \
	elif command -v pwsh > /dev/null 2>&1; then \
		pwsh -File scripts/check-separability.ps1; \
	else \
		powershell -NoProfile -File scripts/check-separability.ps1; \
	fi

# ─── Integration Tests (SystemHarness, no external deps) ─────────────────────

integration:
	go test -tags integration ./internal/testing/harness/... -v

# ─── Per-PR Chaos Scenarios (in-process, Faulty* adapters) ───────────────────

chaos:
	go test -tags chaos ./internal/testing/chaos/... -v -timeout 30s

# ─── Real-Service Chaos (Docker Compose + toxiproxy) ─────────────────────────

chaos-real:
	@echo "--- Starting chaos infrastructure ---"
	docker compose -f scripts/chaos-compose.yml up -d
	@echo "--- Running real-service chaos scenarios ---"
	go test -tags chaos ./internal/substrate/network/... -v -timeout 30m || \
		(docker compose -f scripts/chaos-compose.yml down -v; exit 1)
	@echo "--- Tearing down chaos infrastructure ---"
	docker compose -f scripts/chaos-compose.yml down -v

# ─── Goroutine Leak Detection ─────────────────────────────────────────────────

# Runs goleak TestMain for all background worker packages.
#
# This list must match the packages that actually carry a leak_test.go; a stale
# entry is not harmless. `internal/supervision/clusterer` (retired with the LLM
# CapabilityClusterer, ADR-0067) and `internal/supervision/synaptic` were both
# removed from the tree while still listed here, so `go test` failed with
# "setup failed" on a missing pattern and the target had been red — which nothing
# noticed, because no CI ran it.
#
# If you retire a worker package, delete its line. If you add one, add it here
# together with its leak_test.go.
leak:
	go test ./internal/supervision/aggregator/... -v
	go test ./internal/metabolism/interview/... -v
	go test ./internal/supervision/verify/... -v
	go test ./internal/supervision/circadian/... -v

# Full-kernel goroutine leak test (requires chaos tag)
leak-integration:
	go test -tags chaos ./cmd/orchestrator/... -run TestKernel_NoGoroutineLeak -v

# ─── Benchmarks ──────────────────────────────────────────────────────────────

bench-micro:
	go test -bench=BenchmarkMicro -benchmem ./internal/benchmarks/...

bench-macro:
	go test -bench=BenchmarkMacro -benchmem -benchtime=10s ./internal/benchmarks/...

# Run micros and diff against committed baseline
bench-compare:
	go test -bench=BenchmarkMicro -benchmem -count=5 ./internal/benchmarks/... > /tmp/cambrian-bench-new.txt
	benchstat internal/benchmarks/baseline.txt /tmp/cambrian-bench-new.txt

# Update micro baseline after a legitimate optimisation
bench-update-baseline:
	go test -bench=BenchmarkMicro -benchmem ./internal/benchmarks/... > internal/benchmarks/baseline.txt
	@echo "Baseline updated. Review the diff, then: git add internal/benchmarks/baseline.txt && git commit"

# ─── Fuzzing ─────────────────────────────────────────────────────────────────

fuzz:
	go test -fuzz=FuzzProtoToHandoff -fuzztime=10m ./internal/substrate/network/...

fuzz-release:
	go test -fuzz=FuzzProtoToHandoff -fuzztime=1h ./internal/substrate/network/...

# ─── Agent Contract Tests ─────────────────────────────────────────────────────

contract:
	pytest agents/contract_test.py -v

contract-release:
	bash scripts/run-agent-contract-release.sh

# ─── Data Generation ──────────────────────────────────────────────────────────

corpus:
	go run ./tools/mockgen-cli/main.go \
		-scenario baseline -n 1000 -seed 42 -output synthetic_corpus.jsonl
	@echo "Corpus written to synthetic_corpus.jsonl"

export:
	go run ./tools/export-events/main.go \
		--db data/cambrian.db \
		--output events.jsonl
	@echo "Events exported to events.jsonl"

# ─── Pipelines ───────────────────────────────────────────────────────────────

# Per-PR pipeline: fast, no Docker, runs in <2 minutes
# `test-race` replaces `test` rather than joining it: -race runs the same suite,
# so listing both would pay for the whole thing twice.
#
# The proto gates run FIRST and cost ~2s: a stale binding or a broken wire
# contract invalidates every test that follows, so failing before the 40s race
# suite is strictly cheaper. They were documented as mandatory in the change-
# control rules while being wired into no pipeline at all — the reason a contract
# change once reached main unbumped was that nothing ran the check, not that
# someone bypassed it.
per-pr: proto-check proto-breaking test-race separability integration chaos bench-micro
	@echo ""
	@echo "=== Per-PR pipeline complete ==="

# Nightly pipeline: macro benchmarks + short fuzz + leak detection
nightly: bench-macro fuzz leak deadcode
	@echo ""
	@echo "=== Nightly pipeline complete ==="

# Release gate: all suites, real services, 1h fuzz
release-gate: bench-macro chaos-real contract-release fuzz-release leak-integration
	@echo ""
	@echo "=== Release gate pipeline complete ==="

# ─── Protobuf (ADR-0047 0047-13 / Amendment A2) ───────────────────────────────
# protoc is the CANONICAL generator; buf runs the schema gates (breaking, lint).
# Two different jobs: protoc compiles one version of a schema, buf compares two.
# protoc has no breaking-change mode at all, so it cannot stand in for the gate.
#
# This target used to prefer buf when installed and fall back to protoc, on the
# stated assumption that both reproduce the committed bindings. MEASURED
# 2026-07-31: they do not. buf.gen.yaml's remote plugins are unpinned, so buf
# emits `protoc-gen-go-grpc v1.6.2` / `protoc (unknown)` against the committed
# `v1.6.1` / `protoc v7.34.1`. The embedded descriptor is toolchain-independent;
# the generated file header is not. Preferring whichever tool happened to be on
# PATH made `proto-check` fail on a CLEAN tree the moment someone installed buf —
# same command, different output per machine. One generator, named explicitly.
#
# Canonical toolchain (matches the committed bindings; a mismatch here is what
# proto-check will report):
#   protoc v7.34.1 · protoc-gen-go v1.36.11 · protoc-gen-go-grpc v1.6.1
PROTOC ?= protoc
BUF    ?= buf

proto:
	$(PROTOC) -I api/proto \
		--go_out=api/proto --go_opt=paths=source_relative \
		--go-grpc_out=api/proto --go-grpc_opt=paths=source_relative \
		api/proto/operator.proto api/proto/cambrian.proto

# Drift gate: the committed bindings must match what the .proto files generate
# RIGHT NOW (ADR-0047 A2.7).
#
# Generates to a temp dir and compares, rather than regenerating in place and
# diffing against HEAD. The old form did `proto` then `git diff -- api/proto`,
# which had two faults that only showed once this was wired into `per-pr`:
#
#   1. `api/proto` holds the hand-edited .proto SOURCES as well as the generated
#      .pb.go. An uncommitted proto edit therefore failed the gate — reporting
#      "generated bindings are stale" when the bindings were perfectly in sync —
#      so per-pr could never pass while you were editing a contract, which is
#      precisely when you would run it. It passed in CI only because the edit is
#      committed by then.
#   2. Regenerating in place rewrote four files on every run, leaving the tree
#      stat-dirty on Windows (core.autocrlf=true) even when byte-identical.
#
# Comparing generated-vs-fresh is the property actually wanted, and it leaves the
# working tree untouched.
#
# The comparison is LINE-ENDING AGNOSTIC (`diff --strip-trailing-cr`). protoc
# always emits LF; a Windows checkout under core.autocrlf=true holds CRLF. Those
# are the same bindings, and a byte comparison called them stale — the gate failed
# with "run 'make proto' and commit" when running `make proto` would have changed
# nothing a reviewer could see. A REAL staleness always shows as a content
# difference, which this still catches. See .gitattributes for the underlying fix.
proto-check:
	@tmp=$$(mktemp -d) || exit 1; \
	$(PROTOC) -I api/proto \
		--go_out=$$tmp --go_opt=paths=source_relative \
		--go-grpc_out=$$tmp --go-grpc_opt=paths=source_relative \
		api/proto/operator.proto api/proto/cambrian.proto || { rm -rf $$tmp; exit 1; }; \
	rc=0; \
	for f in operator.pb.go operator_grpc.pb.go cambrian.pb.go cambrian_grpc.pb.go; do \
		if ! diff -q --strip-trailing-cr "api/proto/$$f" "$$tmp/$$f" >/dev/null 2>&1; then \
			echo "stale binding: $$f"; rc=1; \
		fi; \
	done; \
	rm -rf $$tmp; \
	if [ $$rc -ne 0 ]; then \
		echo "generated bindings do not match the .proto sources: run 'make proto' and commit"; \
		exit 1; \
	fi

# Propagate / verify the vendored .proto copies in ui/, cli/ and (report-only)
# cambrian-benchmarks. Requires the sibling repos to be checked out alongside this
# one, i.e. the local workspace layout — which is why this is not in `per-pr`
# (core's own CI clones core alone). The multi-repo gate is the `proto-sync` job in
# .github/workflows/ci.yml, which checks the siblings out first.
proto-sync:
	bash scripts/sync-protos.sh

proto-sync-check:
	bash scripts/sync-protos.sh --check

# Fail on a backward-incompatible operator-contract change.
#
# Fails LOUD when buf is missing instead of skipping. A gate that quietly does not
# run is not a weaker gate, it is a false one: `SetRuntimeConfig` reached main
# without a contract bump while this target existed but was wired into nothing.
proto-breaking:
	@command -v $(BUF) >/dev/null 2>&1 || { \
		echo "buf is required for the operator-contract gate, and is not installed."; \
		echo "    go install github.com/bufbuild/buf/cmd/buf@v1.47.2"; \
		exit 1; \
	}
	$(BUF) breaking --against '.git#branch=main'

# NOT in per-pr: operator.proto carries ~15 pre-existing RPC-naming findings
# (SubmitPlanOpRequest vs SubmitPlanRequest). Renaming them is a contract break,
# so they need an explicit `except` in buf.yaml before this can gate anything.
proto-lint:
	@command -v $(BUF) >/dev/null 2>&1 || { \
		echo "buf is required for proto-lint, and is not installed."; \
		echo "    go install github.com/bufbuild/buf/cmd/buf@v1.47.2"; \
		exit 1; \
	}
	$(BUF) lint

# PLAT-01: regenerate per-agent requirements.txt + the union lockfile from the
# installed agent Python (importlib.metadata — no pip-tools needed).
PYTHON ?= python
agent-reqs:
	$(PYTHON) scripts/gen_agent_requirements.py

# CI drift gate: the committed requirements must match a fresh generation.
# Chains the JS twin (ADR-0125) so one target gates both fleets.
agent-reqs-check: agent-packages-check
	$(PYTHON) scripts/gen_agent_requirements.py --check

# ADR-0125: PLAT-01 twin for JS agents — keep each unit's package.json honest
# against its imports and maintain the union workspace agents/package.json
# (one `bun install` at agents/ = the union-lockfile analog). No-op while no
# JS agent units exist.
agent-packages:
	$(PYTHON) scripts/gen_agent_packages.py

agent-packages-check:
	$(PYTHON) scripts/gen_agent_packages.py --check

# PLAT-02 / ADR-0064: DB migration runner. `migrate` applies the baseline head schema
# + pending forward deltas; `migrate-status` prints the version table. Reads the same
# configs/config.json the kernel boots with.
migrate:
	go run ./cmd/orchestrator migrate up

migrate-status:
	go run ./cmd/orchestrator migrate status
