---
id: 0090
title: Ingress and Surface Identity — Every Point Where the Outside World Enters
status: Proposed
date: 2026-07-26
supersedes: []
superseded_by: []
amends:
  - 0080-chat-daemon-ownership
depends_on:
  - 0007-input-router
  - 0033-daemon-agents
  - 0034-scope-and-classification
  - 0047-operator-transport-plane
  - 0080-chat-daemon-ownership
  - 0084-conversation-model-session-pool
  - 0085-access-policy-port-and-extraction
---

# ADR-0090: Ingress and Surface Identity

## Status

**Proposed — D0/D2/D3/D7/D8/D9/D10/D11/D12/D13 implemented; D4 is not, and the ingress swap is unmeasured.** Renames
ADR-0080's *Chat Manager* tier and generalises it from chat to every inbound signal type.

Shipped: the `domain.IngressResolver` port and the `Session.Surface` writer (D3), the
Postgres-backed ingress registry with namespaces (D2), the rename through code and docs (D0),
and the conversation delivery address with the envelope rule (D7, migration `0006`). The clamp
is armed — a session opened by a registered ingress now carries that ingress's surface, which
nothing previously wrote.

Not shipped: the verified identity link (D4).

**The replacement ingresses are built but NOT measured.** `HTTPChatIngress` and
`AirlineChatIngress` speak the same contract as the Go ingress they replace, and the Go one is
still in the tree, so both paths exist and the driver can be pointed at either. Switching the
airline benchmark over is a **measurement**, not a code change: the data-driven mandate wants an
airline run on the Go path and on the SDK path, with the comparison recorded in DECISIONS.md.
Until that runs, the Go ingress remains the validated path and the SDK ones are untested against
a live kernel.

## Context

### The question that prompted this

*"Surfaces will mostly be daemon agents themselves — a Telegram surface will be a daemon agent,
employees talk to Cambrian from Telegram, and it forwards their messages to the agent that
handles the talk and passes the reply back. Is that a good design?"*

Yes, and it is already the architecture. ADR-0080 D1 specifies exactly this: a long-running
front door that owns the inbound listener, applies user-authored authentication and tenant
routing, forwards each message to the correct per-conversation Session daemon (D2), and
returns the reply — `receive(message, conversation_id, auth) → forward → reply`. A Telegram
front door is that, with a Telegram listener.

Three properties make it a good shape, and they are easy to lose in a redesign:

- **Per-user state is process-isolated.** One employee's session hanging, crashing or being
  restarted (ADR-0070) touches nobody else.
- **Chat never enters the planner's front door** (ADR-0080 D4). A greeting costs a turn, not a
  plan — which is the entire defect ADR-0080 was written to fix.
- **The network code stays user-authored.** Telegram's API, webhooks, rate limits and retry
  semantics live in the user's code, not in the kernel.

### Why this is not a chat concept

Chat is one payload type. A webhook, a websocket listener, an inbound REST call and a cron tick
are the same shape: **something outside Cambrian caused something inside Cambrian to happen.**
The kernel already half-admits this — `SurfaceChat`, `SurfaceReactive`, `SurfaceAgent`,
`SurfaceOperator` and `SurfaceInternal` all exist as surface kinds, and
`execution.ingestion_http_port` is an HTTP entry point today.

Scoping the concept to chat would mean inventing it again for webhooks, and a third time for
websockets, each with its own idea of who the caller is. The concept to name is the **entry
point**, not the payload riding through it.

### Where the current design breaks

INV-5, stated in `internal/authz/surface.go`: *the surface is established by the KERNEL, from
the connection the request arrived on. It is never read from a request payload and never taken
from a daemon's claim about itself. A daemon is a black box; a black box asserting its own
privilege level is not a security boundary.*

Trace a Telegram message. The front door authenticates an employee and forwards to a session
daemon. That daemon queries memory, calls a tool, or yields a subgoal. The kernel derives the
surface from the transport: `SurfaceForMethod` sees the agent plane and returns `agent:grpc`.
So:

1. **The clamp never engages.** A `chat:telegram` surface forbidding `internal_only, secrets,
   PII` is worthless if Telegram traffic is indistinguishable from any other agent-plane call.
2. **The employee is invisible.** The kernel sees the session daemon's agent id. Alice and Bob
   are one principal, and the audit record says so.
3. **The only in-daemon fix is to let the daemon assert its own surface or principal** — which
   is INV-5's prohibition, and the `x-agent-id`-for-a-human violation ADR-0047 exists to
   prevent.

### The gap this surfaced

`domain.Session.Surface` exists and documents itself as *"the entry point this session was
OPENED on, decided ONCE by the kernel"*. `SessionManager.SessionSurface` reads it, and
`authz.ResolveSurface` prefers it over the transport surface for the right reason: a
conversation opened on an outsider entry point must stay an outsider conversation even when a
later turn arrives over an internal path.

**Nothing in the codebase writes that field.** `grep -rn '\.Surface *=' --include='*.go'`
returns nothing; neither `CreateSession` nor `CreateScopedSession` sets it. The read path is
complete and the value is always zero, so `ResolveSurface` unconditionally falls through to the
transport surface. **The mechanism the clamp depends on is built and unarmed.**

This changes what "add Telegram" costs: most of the work below is *arming an existing seam*,
not building a new one.

## Decision

### D0 — Vocabulary: an **ingress**

**An *ingress* is any point at which something outside Cambrian causes something inside
Cambrian to happen.** Telegram, a webhook receiver, a websocket listener, an inbound REST API,
an email poller, a schedule tick. ADR-0080's **Chat Manager** is renamed the **Chat Ingress**,
and becomes one instance of the general concept rather than the concept itself.

Two names were considered and rejected on collision grounds, recorded so this is not
re-litigated:

| Candidate | Why not |
|---|---|
| **Operator** | Load-bearing across 144 files with one meaning: *a human at the console*. `SurfaceOperator = "operator" // the operator console / CLI`, `OperatorConsole`, the Operator-vs-Viewer role — in ADR-0085 it is a **privilege level**. "Telegram operator" reads as *a human operating Cambrian via Telegram*, which is precisely the conflation this ADR forbids. |
| **Router** | Taken twice. `domain/router.go` defines `InputRouter`/`RouterInput` (classify a stimulus into chat/plan/ingest/watch), and "routing" is the Zero-Hardcode Rule's term for agent-to-task assignment. An ingress is deliberately deterministic, so the name would contradict a rule it does not violate. |

`Ingress` has no existing use in `domain/` or `internal/` and generalises without strain:
Telegram ingress, webhook ingress, websocket ingress.

**Why not simply call the daemon a "surface"?** It was considered — the correspondence is 1:1
(D2), so a single word is tempting. Two reasons to keep both:

- **Surfaces exist without daemons.** `SurfaceOperator` is a human at the console,
  `SurfaceAgent` is the gRPC plane, `SurfaceInternal` is an in-process call path. None is a
  process. If "surface" meant the daemon, `SurfaceInternal` would be incoherent. The 1:1 holds
  for *daemon-backed ingress*, not for surfaces in general.
- **The security invariant needs two nouns.** It says *the thing carrying the traffic must not
  assert its own privilege level*. With one word that becomes "the surface must not assert its
  own surface", and the distinction INV-5 rests on disappears into a tautology.

The rule of thumb: **an ingress *has* a surface; it is not one.** The surface is the
kernel-owned identity that appears in policy and audit; the ingress is the daemon holding the
outside connection, assigned exactly one surface.

### D0.1 — The operator console is a surface, NOT an ingress

Asked and decided: the console is another point where a human outside reaches Cambrian, so
should it be registered as an ingress? **No**, and the reason is a failure mode rather than
taxonomy.

An ingress takes its surface from a **registry row**, and that row is edited *through the
console*. Making console access depend on it means one bad registration locks everybody out of
the only tool that can repair it. A bootstrap hazard is a poor trade for conceptual tidiness.

The console also gains nothing. It is already the surface `operator:console`, derived from the
transport — and for the console the transport IS the strongest available fact: an authenticated
human on the kernel's own plane, holding a `Login` token and a role. An ingress registration
would replace a strong, immutable derivation with a weaker, mutable one.

This is why `ResolveIngress` refuses any principal that is not an agent, with a test naming the
rule. The ingress concept exists because a daemon is a black box relaying third parties and must
not assert its own privilege; a console user is not that.

### D1 — An ingress is a daemon, not a plugin

ADR-0080 D1 is right and stands. The listener, retry semantics and tenant routing are user code
and belong outside the kernel. Compiling ingress into the kernel would drag every chat network
and webhook dialect into the kernel's build and release cycle for no security gain — **the
trust problem is not where the code runs, it is who is believed.**

### D2 — One ingress ↔ one surface, and the kernel owns the mapping

**The ingress daemon *is* the surface** — the correspondence is 1:1, which is the direct answer
to "will the surface be that daemon itself?". What the daemon does *not* get is the right to
say which surface it is.

The mapping from ingress to surface is **registered out of band by an operator**, once, in
configuration or policy. The kernel attaches that identity to everything arriving through that
ingress. The daemon carries the traffic; the kernel carries the identity. This is the whole
difference between *"the kernel knows the Telegram ingress is at the end of this connection"*
and *"something at the end of this connection says it is Telegram."*

### D3 — The surface is stamped onto the session at open time

The existing `Session.Surface` seam, armed. When a session is opened on behalf of a registered
ingress, the kernel writes that ingress's surface onto the session record. `ResolveSurface`
already prefers it over the transport surface. The daemon delivering turn 40 cannot restate the
surface, because it was decided at turn 0 and is read server-side.

**Only a registered ingress stamps, and that restriction is load-bearing.** `ResolveSurface`
prefers the session's surface over the transport's on the assumption that the stored one is
*narrower*. That holds for an outsider-facing ingress. It is false for an operator-created
session, whose surface is the most privileged there is — so stamping every session with
whatever opened it would silently WIDEN ordinary sessions, turning a safety mechanism into an
escalation. Restricting the stamp to registered ingresses makes the narrowing assumption true
by construction, and leaves every existing path exactly as it was.

### D4 — The end-user principal requires a link the kernel verified

A Telegram numeric id, a webhook's `X-User` header and a websocket query parameter are
identifiers, not authentications. Ranked:

1. **Verified link (target).** The person proves the connection through an already-trusted
   channel — signs into the console, receives a one-time code, presents it at the ingress. The
   kernel stores `(surface, external_id) → principal`. The ingress forwards only the external
   id; the kernel resolves it.
2. **Single guest principal (until then).** Every user of that ingress resolves to one
   principal, e.g. `telegram-guest`, whose boundary is the intersection of its own scope and
   the surface clamp.

Option 2 is honest: the deployment genuinely cannot tell people apart over that ingress yet,
and says so, rather than trusting the ingress's word and producing an audit trail that reads as
authoritative while being decorative. **Never** infer a principal from an unverified external
id — that is authentication by assertion, and it makes every downstream policy decision a
fiction.

### D5 — Composition guarantees the failure direction

Every fold in the policy model is an intersection (ADR-0087). So as long as the surface clamp
is linked, a missing or wrong user identity can only ever **narrow** what is reachable, never
widen it. That is what makes shipping D4 option 2 acceptable in the interim rather than
reckless.

### D6 — Ingress dispatch is a sanctioned Zero-Hardcode exception

An ingress is deterministic by design: an LLM deciding which chat a message belongs to, or
which surface a connection is on, would be both slower and unsafe. This is recorded as a
**named exception** rather than an oversight, following the precedent of ADR-0007's reflexive
path and ADR-0031's documented exception.

It qualifies on two of the three existing grounds:

- **Transport demux is not agent-to-task routing.** The exception for system-shell commands
  already turns on this distinction — *"the shell is the OS layer, not the agentic substrate;
  user-to-system-mode routing is not agent-to-task routing."* Mapping a Telegram chat id to its
  conversation is the same class of decision: it is addressing, not delegation.
- **Identity is a security gate, and security gates must be deterministic.** The third
  exception exists precisely so an LLM cannot talk its way past scope, approval or budget. An
  ingress establishes *which surface* and *which principal* — the inputs to every later
  decision. An LLM in that path would be a prompt-injection target with authority.

**The boundary of the exception, which is what keeps it safe:** an ingress may decide *which
conversation* a message belongs to and *which surface* it arrived on. It may **not** decide
*which agent* handles the work. Which agent runs a session daemon is configuration; escalation
to the planner goes through `yield_subgoal` and the auction as normal. If an ingress ever
selects an agent by capability, that is a genuine Zero-Hardcode violation and not covered here.

### D7 — The delivery address is envelope data the kernel owns

A conversation carries where its replies go: `DeliveryAddress{IngressAgentID, ExternalID}`,
bound by the kernel on first inbound contact.

**An agent names a conversation; it never names a recipient.** This is SMTP's envelope-versus-
content split. Fire-and-forget delivery means the ingress will send wherever it is told, so if
the outbound message carried its own recipient then anything able to produce a message could
choose who reads it — an agent could address internal data to an attacker's chat id and the
ingress would dutifully deliver. Resolving the address from the conversation costs the ingress
nothing (it still just receives an address and sends) and removes the choice from the caller.

**Binding is write-once.** Rebinding is a redirect, which is the single operation an attacker
who reached this call would want: it silently retargets every future reply. So an established
address survives, and changing a recipient is a deliberate administrative act rather than a
side effect of a message arriving. The guard is in the `UPDATE ... WHERE delivery_ingress = ''`
clause rather than a read-then-write, because two inbound messages racing on first contact
would both see "unbound" and the second would win. Proved with a 12-way concurrent test against
real Postgres: exactly one bind succeeds, every other caller is told it was already bound.

**The namespace is enforced at bind time** (`IngressRegistration.DeliveryFor`), so an ingress
can only ever bind identities it was registered to speak for. Once bound, delivery resolves the
address from the conversation rather than from anything a caller supplies — so an identity that
was never bound can never be delivered to.

**An unbound conversation is undeliverable, and that is correct.** A console-only conversation
has no ingress address, so nothing can be pushed to it. This matches Telegram's own rule that a
bot cannot open a chat with someone who never contacted it: the security constraint and the
platform constraint agree.

### D8 — Outbound delivery is a separate one-way flow

Inbound ends the moment the kernel accepts a signal. Outbound is its own flow that may happen
never, once or many times, seconds or hours later. Nothing correlates them: an agent that says
three things produces three deliveries, and `internal/ingress.DeliveryService` is the single
place a conversation id becomes a recipient.

The order of operations is the design — **resolve, re-authorise, then send**:

- **Re-authorise at delivery time, not only at bind time.** An address bound yesterday through
  an ingress that has since been revoked, or whose namespace was narrowed, must stop
  delivering. Checking only at bind would mean revoking a compromised ingress prevented new
  conversations while every existing one kept working — which is the opposite of what
  revocation is for.
- **Idempotency before the send.** A retried delivery must not produce a second message on the
  far side. The key is the message id (Matrix's transaction ids): delivering message M is the
  same act however many times it is attempted. A journal that cannot answer **refuses** rather
  than sending — late beats twice.
- **Permanent versus transient is the ingress's call**, because only it knows whether a 403 is
  "blocked" or "rate-limited". Anything unlabelled is treated as transient: wrongly retrying
  costs one duplicate attempt, wrongly giving up loses the message. Retrying a permanent
  failure forever is how an integration gets banned.
- **Permanent failures are dead-lettered.** This is the honest cost of asynchrony — a reply
  that fails after its turn already succeeded has nowhere to surface, where a synchronous
  design would have returned the error to the caller.

`DeliveryJournal` is nil-safe: without it delivery still works, retries may duplicate, and an
undeliverable message is logged rather than recorded. Best-effort is a legitimate v1 posture as
long as it is stated rather than discovered.

`DaemonTransport` is the only place this becomes a process call, over the existing `CallDaemon`
seam. An ingress is ONE long-lived daemon, unlike a per-conversation session daemon, so it is
addressed by its agent id — overridable, not hardcoded, for deployments running several
instances.

### D9 — One inbound message does not imply one outbound message

`TurnService.Turn` returns exactly one `domain.Message`, and that single return value was the
whole blocker on the duplex model: a function returning one message can never say
"checking..." and then answer, and can never speak unprompted.

Rather than change `Turn`'s signature — it has one caller, the synchronous console, which
legitimately wants exactly one reply — the speak primitive is separated out. `TurnService.Emit`
appends a message and, when the conversation arrived through an ingress, delivers it. `Turn`
now routes its reply through `Emit`, so the console path is unchanged while anything may call
`Emit` any number of times, including with no inbound turn at all.

Two properties fall out that are worth stating:

- **The stored message is durable before delivery is attempted**, so a delivery failure never
  loses what was said. `Emit` returns the message *and* a delivery error; the message is valid
  either way, and `Turn` deliberately does not fail a turn because delivery failed — the reply
  is stored, the console still gets it, and the undeliverable copy is dead-lettered.
- **"No delivery address" is not an error.** It is every conversation in a console-only
  deployment.

### D10 — The SDK gains an ingress base class

Neither existing base class could express an ingress, which is why the concept needed one:
`DaemonAgent` produces signals but is never called (no delivery path), and `CognitiveAgent` is
called but produces none (no inbound path). `IngressAgent` does both — it serves the gRPC
endpoint the kernel delivers to, on the daemon's existing UDS socket, and holds the signal
stream it pushes inbound traffic onto. The stream runs on a background thread and the server
owns the process lifetime, so an ingress that can no longer poll can still deliver.

Authors implement two methods, one per direction: `listen()` (supervised, never returns) and
`on_deliver(recipient, text, conversation_id)` (no return value, because nothing is correlated
with it). `PermanentDeliveryError` is how an ingress says "never retry"; anything else is
transient, because only the ingress knows whether a 403 means blocked or rate-limited.

**The class has no way to declare a surface and no way to choose a recipient.** That is INV-5
enforced by omission rather than by documentation — an API that cannot express the unsafe thing
is a stronger guarantee than one that documents it.

### D11 — The inbound half, and why it never reaches the planner

Delivery (D8) was only half of it. A message arriving from an ingress travels the ordinary
agent signal stream, and `Server.SignalStream` previously had two destinations: the OSS Watcher
(which presents signals to the **planner**) or the reactive engine. Neither is right.

An ingress message is a conversational turn, so it goes to the chat lane. This is ADR-0080 D4
enforced for external traffic, and D4 exists because turns that reached the planner were
decomposed into steps like *"ask the customer to provide their booking reference"* — which no
agent can execute, so step 0 failed, replan regenerated it, and the failure text was emitted as
the spoken reply. `SignalStream` therefore checks for a registered ingress **first** and ends
the iteration; the planner never sees it. An unregistered sender returns `ErrNotAnIngress` and
falls through untouched, so ordinary agent signals are unaffected.

`InboundService.Accept` authorises the sender, then resolves the conversation, then runs the
turn — in that order, so a message refused for being outside its namespace leaves no orphan
conversation behind. On first contact it opens a conversation with the delivery address bound in
the same write, which is why the very first reply already knows where to go.

Two decisions inside it:

- **The conversation owner is `ingress:<agent>:<external-id>`.** Per-sender rather than
  per-ingress, so two people on one bridge do not share a transcript owner. It is deliberately
  a pseudonym, not an identity: the external id is unverified until D4's linking table exists,
  so it isolates without proving anything.
- **A closed conversation is not silently reopened.** A new message starts a new one, so an
  ended transcript stays ended.

### D12 — The Go chat ingress is replaced by SDK ingresses

`HTTPChatIngress` in the SDK serves the same three endpoints the Go ingress did — `/open`,
`/turn`, `/close` — so existing callers need no change, and `AirlineChatIngress` in premium is
that class with the airline namespace and a refusal to start unconfigured.

The interesting part is where **correlation** lives. The caller blocks on `/turn` expecting the
reply in the response body, while the core is two fire-and-forget flows. The ingress bridges
those shapes: it sends the message inbound and waits on a per-conversation future that
`on_deliver` completes. That is the right place for it, because request/response is a property
of *that external protocol*, not of Cambrian — putting the waiting in the kernel would drag
every ingress back into a round trip for the sake of the one that needs it.

Where the protocol cannot carry the model, the limit is stated rather than hidden: an HTTP
response holds one reply, so if an agent speaks twice the second is queued for the next request
instead of being dropped.

Placement follows the no-benchmark-logic rule: the generic class ships in the SDK, and
everything airline-shaped is configuration of it, living in premium beside the airline session
agent.

### D13 — Registration is administered on the policy plane

`ListIngresses` / `RegisterIngress` / `DeregisterIngress` on the premium
`AccessPolicyAdmin` service, behind the same operator auth as policy administration.

That placement is not filing convenience. **Registering an ingress mints a surface** — it
decides what an entry point is permitted to reach — so it carries policy-grade authority and
gets policy-grade protection rather than a separate, softer door. It is also why the risk
section says whoever can write `authz_ingress` can mint a surface.

Validation failures are `InvalidArgument`, not `Internal`, because each is an administrator
mistake with a specific fix: a registration with no surface stamps nothing, and an empty
namespace prefix would read as a wildcard. Refusing both here is what stops either being
discovered later as "the bot just doesn't answer". An unconfigured kernel answers
`Unimplemented` rather than pretending to have stored something.

**This closed a gap that made everything above unreachable.** The table, the registry and the
Go API all existed, but nothing exposed them — so no daemon could become an ingress, no inbound
message became a conversation, and the only way in was a manual `INSERT` plus a restart, since a
raw insert does not fire the `pg_notify` the registry listens for. Built is not the same as
usable, and that distinction was invisible until someone tried to use it.

> **Pending amendment — the duplex model.** D1/D2/D3 above are unaffected, but this ADR's
> description of ingress traffic as `receive → forward → reply` was inherited from ADR-0080 D1,
> which specified the MVP's synchronous HTTP driver. That framing is wrong for real ingress:
> the Telegram Bot API acknowledges an update and sends replies through a separate outbound
> call, so an ingress is a **transceiver** — two independent fire-and-forget flows, not a
> correlated round trip. Consequences already worked through: an agent must name a
> *conversation* rather than a raw address (the kernel resolves the envelope, closing an
> exfiltration path), delivery needs a journal because it can now fail after a turn succeeded,
> and `TurnService.Turn` returning exactly one `domain.Message` is the single blocker. To be
> folded in as D7–D9.

## Consequences

**The topology does not change; the direction of trust does.** Identity flows kernel-outward,
never daemon-inward.

**One concept covers every signal type.** Adding a webhook receiver or a websocket listener
later reuses the ingress registration, the session stamp and the linking table rather than
inventing a parallel notion of "who is calling".

**The audit trail becomes meaningful for external traffic.** Today every Telegram user would
appear as one agent principal on the agent surface. With D3+D4 the record names the surface
always, and the person once linking exists.

**A customer-facing entry point becomes shippable before multi-tenancy.** That was already the
stated purpose of the clamp (ADR-0085 D7); D3 is what makes it true for daemon ingress rather
than only for direct connections.

**Cost was mostly arming, not building** — as predicted. `Session.Surface`, `SessionSurface`,
`ResolveSurface` and the `ContainerSurface` link type already existed; what shipped was a
writer, a registry and a port. Still missing: the linking table (D4) and the outbound path.

**A rename with a small blast radius.** In prose, ADR-0080's *Chat Manager* becomes *Chat
Ingress*. In code the affected symbols are few — `cambrian-premium/chat/manager.go`, the
`chat-manager` lifecycle name, the `chat-ingress-session-contract.md` design doc, and two
`FromAgent: "chat_manager"` string literals. The last of those is a **stored wire value** that
appears in persisted conversation rows, so renaming it is a data-migration decision, not a
cosmetic one, and is deliberately left out of this ADR.

## Risks and known gaps

**~~The unwritten `Session.Surface` field~~ — closed.** The field now has a writer, and a
session opened by a registered ingress carries its surface. The residual is narrower: a
deployment that registers no ingress still resolves every surface from the transport, which is
correct but means the clamp does nothing until something is registered.

**The registry can mint a surface.** Whoever can write `authz_ingress` decides what an entry
point is permitted to reach, which is why it lives in the policy store and is protected as
policy-grade data rather than as configuration.

**SEC-03 remains the ceiling.** Every claim here about transport-derived surfaces holds on
localhost and degrades off it. An ingress talks to the public internet by definition, but the
ingress→kernel hop need not: keep the ingress on the same host as the kernel. Running it
elsewhere is a SEC-03 prerequisite, not a deployment preference. This ADR inherits that limit
rather than changing it.

**The linking table is an authentication system, and that is where this gets expensive.**
One-time codes, expiry, revocation on offboarding, and "the employee changed their Telegram
account" are all real work. D4 option 2 exists so an ingress can ship before that work is done —
honestly, rather than by pretending.

**A registered ingress is a trusted registration.** D2 moves trust from the daemon's claim to an
operator's configuration. That is a large improvement and not a total one: whoever can edit the
ingress registry can mint a surface. It should be treated as policy-grade configuration, with
the same protection as the policy store itself.

**The exception in D6 is a boundary that must be policed.** Sanctioned exceptions erode: the
easiest future mistake is an ingress that starts choosing agents "just for this one case". The
stated boundary — conversation and surface yes, agent no — is the thing to check in review.
