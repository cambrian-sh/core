#!/usr/bin/env bash
# Release gate: Agent protocol contract test against real Substrate.
# ADR-0020-C-02
set -euo pipefail

echo "=== Building orchestrator ==="
go build -o /tmp/cambrian-test ./cmd/orchestrator

echo "=== Starting orchestrator ==="
/tmp/cambrian-test --config configs/test-config.json &
CAMBRIAN_PID=$!

# Trap registered immediately after backgrounding — fires even on health check failure.
trap 'kill $CAMBRIAN_PID 2>/dev/null; wait $CAMBRIAN_PID 2>/dev/null' EXIT

echo "=== Waiting for gRPC health check (max 6s) ==="
ATTEMPTS=0
until grpc_health_probe -addr=localhost:50051 2>/dev/null || [ $ATTEMPTS -ge 60 ]; do
  sleep 0.1
  ATTEMPTS=$((ATTEMPTS+1))
done
if [ $ATTEMPTS -ge 60 ]; then
  echo "ERROR: orchestrator failed to start after 6s"
  exit 1
fi

echo "=== Running agent contract tests ==="
pytest agents/contract_test.py --real-substrate -v

echo "=== Done ==="
