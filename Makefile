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

test-race:
	go test -race ./internal/...

# ─── Separability Gate ───────────────────────────────────────────────────────

separability:
	@echo "--- Checking OTel / premium separability ---"
	@if command -v pwsh > /dev/null 2>&1; then \
		pwsh -File scripts/check-separability.ps1; \
	else \
		! grep -r "go.opentelemetry.io" \
			internal/domain internal/metabolism internal/awareness \
			internal/supervision internal/substrate 2>/dev/null | grep .; \
		! grep -r "internal/premium" \
			internal/domain internal/metabolism internal/awareness \
			internal/supervision internal/substrate 2>/dev/null | grep .; \
		echo "PASS: No OTel or premium imports in core packages"; \
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

# Runs goleak TestMain for all background worker packages
leak:
	go test ./internal/supervision/aggregator/... -v
	go test ./internal/supervision/clusterer/... -v
	go test ./internal/metabolism/interview/... -v
	go test ./internal/supervision/verify/... -v
	go test ./internal/supervision/synaptic/... -v
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
per-pr: test separability integration chaos bench-micro
	@echo ""
	@echo "=== Per-PR pipeline complete ==="

# Nightly pipeline: macro benchmarks + short fuzz + leak detection
nightly: bench-macro fuzz leak
	@echo ""
	@echo "=== Nightly pipeline complete ==="

# Release gate: all suites, real services, 1h fuzz
release-gate: bench-macro chaos-real contract-release fuzz-release leak-integration
	@echo ""
	@echo "=== Release gate pipeline complete ==="

# ─── Protobuf (ADR-0047 0047-13 / Amendment A2) ───────────────────────────────
# Generate + commit Go bindings via buf, falling back to protoc when buf is
# absent (offline contributors). Both must reproduce the committed bindings — the
# proto files pin `option go_package = "core/api/proto"` so the
# embedded descriptor is toolchain-independent. `proto-check` guards drift.
proto:
	@if command -v buf >/dev/null 2>&1; then \
		buf generate; \
	else \
		echo "buf not found — falling back to protoc"; \
		protoc -I api/proto \
			--go_out=api/proto --go_opt=paths=source_relative \
			--go-grpc_out=api/proto --go-grpc_opt=paths=source_relative \
			api/proto/operator.proto api/proto/cambrian.proto; \
	fi

# CI drift gate: regenerating must not change the committed bindings (ADR-0047 A2.7).
proto-check: proto
	@git diff --exit-code -- api/proto || (echo "generated bindings are stale: run 'make proto' and commit"; exit 1)

# Fail on a backward-incompatible operator-contract change (CI gate).
proto-breaking:
	buf breaking --against '.git#branch=main'

proto-lint:
	buf lint
