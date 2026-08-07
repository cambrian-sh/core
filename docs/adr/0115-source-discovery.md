
## Addendum (2026-08-06): R-ITEM-IDENTITY — item identity is derived, never asked

A poller re-READS its source, so the property that decides whether the archive
stays sane is: does a re-fetch of unchanged data dedup? Nothing verified it.
Two armed discovery-authored ingresses shipped without item identity and the
live consequences were measured: 282,522 evidence rows for 821 distinct bodies
(USGS — random per-tick refs), and one whole-body poller archiving every fetch
because a per-request `generationtime_ms` sat beside a stable payload
(open-meteo).

Like the archetype ("never asked of the agent — a fact the session already
holds"), item identity is a property of the SOURCE, derivable from evidence:
two fetches of the proposed handle. `Session.Propose` now runs
`verifyItemIdentity` (`sourcediscovery/identity.go`) for poller proposals —
probing the handle once more through the validated pipeline when the session
holds only one fetch — and:

- **derives `delivery_ref_path`** when items carry a field unique within a
  page and stable across fetches (name-ranked: `id` first). An identity links
  REVISIONS of one item, which content-derived refs cannot.
- **derives `collection_root`** when a whole-body response is
  envelope-volatile around a stable subtree — the subtree is what the item
  IS, and pointing the transport there keeps envelope noise out of the item
  bytes. (`splitItems` accepts an object-valued `items_path` as one item.)
- **verifies a declared `delivery_ref_path`**: must resolve on every item, be
  unique within a fetch, and identify the same items across fetches — a
  declared identity that fails these LOOKS deduplicating and is not.
- **refuses** only the unfixable shape: every item changed between two
  fetches seconds apart and nothing identifies them — that poller re-archives
  its entire feed every tick. The refusal names the volatile paths.
- Empty collections and non-JSON/truncated bodies record an honest
  "unverified" finding and pass — the rule refuses defects it can prove, not
  gaps in its own evidence.

Every derivation lands in the epistemic record as an `Observed` finding with
`RuleID: R-ITEM-IDENTITY` citing both probe records. Hand-authored transports
saved directly through `SaveTransportSpec` skip this rule (they have no probe
session) — a capture-sample variant is the residual.
