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

- ENG-2102: `+ runTrace: nil` (Kernel, default) and `+ runTrace: *runtrace.Writer`
  (Kernel, all_on): the kernel holds the run-trace file so Shutdown can close it.
- ENG-2241: `- ArtifactOutputDir: string(set)` (ToolExecutor, both goldens): the
  "_artifacts" promotion to the run ledger and data/outputs went with the native
  tool.py handler, its only producer (finishing ENG-1421).
- ENG-1421: `Handler: *proc.ProcessHandler` -> `Handler: nil` (ToolExecutor, both
  goldens): the native tools/*tool.py process handler was deleted with its
  discovery; tools come only from MCP (and kernel tools / local: steps keep their
  own paths), so the executor has no default handler.
- ENG-117: `+ untrustedOutput: *domain.UntrustedOutputGuard` (ToolExecutor, both
  goldens), the MCP response injection scan built from execution.tools.output_scan
  (default on) and passed as ToolExecutorControls.UntrustedOutput.
- ENG-531: `+ ruleOfTwo: *domain.RuleOfTwoGate` (ToolExecutor, both goldens), the
  Rule-of-Two gate built by `app/rule_of_two.go` from execution.tools.rule_of_two
  (default on) and passed to `NewToolExecutor` as a constructor control.
- ENG-2232: `+ freeQueryOpen: false` (ToolExecutor, both goldens), the free-query
  guard's constructor control (ToolExecutorControls.FreeQueryOpen), false because
  execution.tools.free_query.require_explicit_grant defaults to true.
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
- ENG-1654: `+ DeadLetterRetrier` (Kernel, the late-bound `deadLetterRetrierHolder`) and
  `+ RegisterDeadLetterRetrier` (KernelServices) — the premium reactive engine registers the
  replay behind RetryWatchDeadLetter (failed deliveries only).
