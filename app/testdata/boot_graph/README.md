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

- ENG-2138: `+ Heartbeats` and the `daemon-heartbeat-watch` lifecycle (65bfdf5,
  ENG-1348); `+ Derivations` (a202b04, ENG-2122).
