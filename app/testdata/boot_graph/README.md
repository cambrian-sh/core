# Boot graph goldens

`default.golden` and `all_on.golden` pin the object graph `app` assembles at boot
(`TestPg_BootGraphCharacterization`, Postgres via `PG_TEST_DSN`; ENG-1123).

Regenerate only for an intended wiring change:

    go test ./app/ -run TestPg_BootGraph -count=1 -update-boot-graph

A golden change must name its cause: the commit that changes the graph updates the
goldens in the same change, and its message (or the ticket log) names every added,
removed or changed line and the change that caused it. A diff you cannot attribute
to a landed change is a wiring regression, not a golden to regenerate.

History of golden updates:

- ENG-117: `+ untrustedOutput: *domain.UntrustedOutputGuard` (ToolExecutor, both
  goldens), the MCP response injection scan built from execution.tools.output_scan
  (default on) and passed as ToolExecutorControls.UntrustedOutput.
- ENG-531: `+ ruleOfTwo: *domain.RuleOfTwoGate` (ToolExecutor, both goldens), the
  Rule-of-Two gate built by `app/rule_of_two.go` from execution.tools.rule_of_two
  (default on) and passed to `NewToolExecutor` as a constructor control.
- ENG-2169: `+ PipelineDrainerSliceTasks: int=64` and `+ PipelineDrainerWorkers: int=4`
  (KernelServices, both goldens), mirroring execution.pipelines.drainer_slice_tasks /
  drainer_workers for the premium drainers (round-robin per pipeline, bounded workers).
- ENG-2117: `KernelTools len=9` -> `len=10` and `+ company_records *records.tool`
  (both goldens), the read-only company-records kernel tool wired by
  `wireCompanyRecords` (execution.tools.company_records, default on).
- ENG-2164: `+ PythonEnv` (KernelServices), added by a0d6c62 (ENG-2137, plugin
  daemon spawns wait until the Python env is ready) without regenerating the goldens.
- ENG-2138: `+ Heartbeats` and the `daemon-heartbeat-watch` lifecycle (65bfdf5,
  ENG-1348); `+ Derivations` (a202b04, ENG-2122).
- ENG-2135: `+ serving` (Kernel) and `+ Serving` (KernelServices) — the "kernel is
  serving" signal closed right before GRPC.Serve, which plugins' boot-time daemon
  spawns wait on through `KernelServices.AwaitSpawnReady`.
