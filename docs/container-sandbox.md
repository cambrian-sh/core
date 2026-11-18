# Agent sandbox in a container (bubblewrap in the kernel image)

The kernel image ships **bubblewrap** (`bwrap`). With `execution.agents.sandbox.enabled: true`
(the default since ADR-0157 S7, ENG-2010), the native Linux runner uses it to confine every kernel-spawned agent (ADR-0157 D1): the root
read-only, writes only to the agent's scratch, the kernel's state (the kernel directory,
`configs/` with `config.db` and `secret.key`, `/app/state`, the data directory, the work roots)
masked, and no network for a kernel-only tier. The image's kernel runs as the non-root user
`cambrian`, so bwrap needs an **unprivileged user namespace**, and most container engines forbid
that by default. This page lists the settings that allow it. (ENG-2062; owner decision
2026-11-18: ship bwrap and document the settings, no docker socket in the kernel container.)

## What happens without the settings

The kernel probes bwrap with the same mounts and namespaces an agent launch uses. When the probe
fails, the containment report in the boot log names the controls as **missing**, with bwrap's own
error and a pointer to this page:

```
containment first-party/native ... missing [fs_confine: bubblewrap is installed but cannot create
namespaces here: bwrap: No permissions to create new namespace ... (in a container: allow
unprivileged user namespaces, e.g. docker --security-opt seccomp=deploy/seccomp-bwrap.json; see
docs/container-sandbox.md), hide_state: ..., network: ...]
```

As on a bare-metal host: first-party agents are **degraded** (they start, the report says so),
third-party agents are **refused** (`third_party.on_unavailable: refuse`). Nothing reports a
control as enforced when it is not.

Third-party agents are refused even **with** the settings: their tier also wants
`low_privilege` (a uid other than the kernel's), and a non-root kernel has no lower uid to switch
to. That is the same rule as for a non-root kernel on bare metal.

## Docker

Run the kernel container with the seccomp profile shipped in this repository:

```sh
docker run --security-opt seccomp=cambrian-core/deploy/seccomp-bwrap.json ... ghcr.io/cambrian-sh/core
```

```yaml
# docker compose (the workspace docker-compose.yml does this for the kernel service)
services:
  kernel:
    security_opt:
      - seccomp=./cambrian-core/deploy/seccomp-bwrap.json
```

`deploy/seccomp-bwrap.json` is moby's default profile (v27.5.1) unchanged plus **one rule**
allowing `unshare`, `clone`, `mount`, `umount2` and `pivot_root`. This is the narrowest set found:
each was removed in turn and bwrap then failed (`unshare` → "No permissions to create new
namespace"; `clone` → "unshare user ns"; `mount` → "Failed to make / slave"; `umount2` →
"unmount old root"; `pivot_root` → `pivot_root: Operation not permitted`). The container still has
no `CAP_SYS_ADMIN`, so the mount calls succeed only inside a user namespace the process created;
the cost is the kernel attack surface of unprivileged user namespaces, the same as on a host that
allows them. Measured on Docker 27.5.1 (Linux kernel 5.15) with the kernel's exact launch flags,
also together with `--cap-drop ALL`, `--security-opt no-new-privileges` and `--read-only`.

Not needed: `--privileged`, `--cap-add SYS_ADMIN`, `seccomp=unconfined`,
`systempaths=unconfined` (bwrap's fresh `/proc` mounts under Docker's masked `/proc` there).

Keep the profile in step with the engine: regenerate it from the engine's own default profile when
you upgrade Docker (take `profiles/seccomp/default.json` of that release and append the rule).

Some Docker Desktop installations run containers without the default seccomp profile
(`docker info` shows `seccomp,profile=unconfined`); bwrap then works without any flag. Production
Linux hosts use the default profile, so set the flag anyway.

### AppArmor hosts (Ubuntu, Debian with AppArmor)

The `docker-default` AppArmor profile denies `mount`, so bwrap also fails there with the seccomp
profile alone. Either run the kernel with `--security-opt apparmor=unconfined`, or load a profile
derived from `docker-default` that drops `deny mount,` and add `--security-opt
apparmor=<that profile>`. On Ubuntu 23.10 and later also check the host sysctl
`kernel.apparmor_restrict_unprivileged_userns` (1 blocks unprivileged user namespaces for
processes without an AppArmor profile that allows `userns`). **Not yet verified on an AppArmor
host** (the measurement machine has no AppArmor); the boot report shows whether it worked.

The host must allow user namespaces at all: `user.max_user_namespaces` greater than 0 (and on older
Debian kernels `kernel.unprivileged_userns_clone=1`).

## Podman

Podman's default seccomp profile is not Docker's. If the report names `fs_confine` missing, pass the
same file: `podman run --security-opt seccomp=cambrian-core/deploy/seccomp-bwrap.json ...`; on an
SELinux host the container's label may also need `--security-opt label=disable` or a policy that
allows the mounts. **Not yet verified.**

## Kubernetes

Copy `deploy/seccomp-bwrap.json` into each node's kubelet seccomp directory (default
`/var/lib/kubelet/seccomp/`, e.g. as `cambrian/seccomp-bwrap.json`) and reference it from the
kernel pod:

```yaml
securityContext:
  seccompProfile:
    type: Localhost
    localhostProfile: cambrian/seccomp-bwrap.json
  # AppArmor nodes (Kubernetes 1.30+): a profile that allows mount in a user namespace, or
  # appArmorProfile: {type: Unconfined}
```

`RuntimeDefault` (and the kubelet's `SeccompDefault`) blocks bwrap like Docker's default.
`allowPrivilegeEscalation: false` and dropping all capabilities are fine (bwrap is not setuid in
the image). **Not yet verified.**

## Checking a deployment

The boot log has one containment line per agent. With the settings in place a first-party agent
reports `fs_confine`, `hide_state` (and `network`) enforced by `bwrap`. The image's own test,
`TestE2E_ContainerContainmentReport` in `internal/metabolism/agentmgr`, asserts both shapes
(`AGENTMGR_E2E_EXPECT_CONTAINMENT=enforced|missing`).

The agent sandbox itself (`execution.agents.sandbox.enabled`) is on by default since the
ADR-0157 S7 live run (ENG-2010). Without the settings above, first-party and system agents
start with the missing controls named (their tier degrades by default) and third-party agents
are refused. `execution.agents.sandbox.enabled: false` (environment
`CAMBRIAN_EXECUTION__AGENTS__SANDBOX__ENABLED=false`) restores the launch from before ADR-0157.

The agents' shared model cache (`execution.agents.model_cache_dir`, default
`<kernel dir>/runtime/models`, here `/app/runtime/models`) is where the kernel points
`HF_HOME`, `TORCH_HOME` and `DOCLING_CACHE_DIR` for every agent; it is bound writable for
first-party and system agents and read-only for third-party ones. Pre-fetch models into it
(setup does; in an image, build with `HF_HOME=/app/runtime/models/huggingface`): an agent
under `network: kernel_only_all` cannot download.
