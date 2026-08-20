-- 0016_entities_links.sql — the identity plane (five-planes step 1+2, FIVE-PLANES-BUILD.md).
--
-- The substrate could already say WHAT happened (0014 events/observations) and WHO a
-- record is about (0015 parties), but it had no way to say that two names are the same
-- thing, or that one thing stands in a stated relation to another. `entity_id` was bare
-- TEXT everywhere: a match across two sources was a string coincidence, not a claim
-- anybody made. These two tables are where that claim finally gets a row.
--
-- ENTITIES ARE CHEAP AND MEAN LITTLE. An entity row is a minted handle — "there is a
-- thing called customer/C-1042" — nothing more. It asserts no properties, carries no
-- belief, and costs one insert. All the epistemics live next door in `links`, which is
-- what lets a producer mint freely and still leave every equivalence reviewable.
--
-- A LINK IS AN ASSERTION, NOT A FACT. Every row records who said it (`asserted_by`), by
-- what means (`mechanism`), on what basis (`evidence_id`), when they said it
-- (`asserted_at`) and when the world it describes held (`valid_from`/`valid_to`) — the
-- same shape ADR-0121 gave records and ADR-0108 gave occurrences. `state` is the review
-- lane: a machine producer proposes `candidate`, a human promotes it to `confirmed`.
--
-- TWO PRODUCERS AGREEING IS TWO ROWS, and that is the point. Corroboration is
-- information; collapsing an id-shape heuristic and a declared crosswalk into one row
-- would destroy exactly the signal a reviewer needs. The dedup key therefore includes
-- `mechanism` and `source_ref` — one producer replaying is a no-op, two producers
-- agreeing is two rows, and READ paths deduplicate.
--
-- NOTHING IS EVER DELETED. A rejected candidate stays queryable (`state='retracted'` +
-- `retracted_at`), because a producer that cannot see its proposal was rejected will
-- propose it again on the next run. The store models the transition as an UPDATE of
-- `state`/`retracted_at` ONLY — no other column of an existing row may ever be
-- rewritten (ADR-0093 D6).
--
-- `relation` IS DATA. The kernel never branches on a verb; verbs are declared in the
-- boot-time RelationRegistry (domain/identity.go) and the store validates against it.
-- Symmetry and closure are properties the registry states, not names in kernel code.
--
-- IDENTITY IS CANONICALLY ORDERED. For family='identity' the store enforces
-- from_ref < to_ref, so "A same_as B" and "B same_as A" are one row rather than two
-- that no dedup key could ever reconcile. Symmetry is a read-path property.
--
-- ADDITIVE. Rollback: DROP TABLE links; DROP TABLE entities;

CREATE TABLE IF NOT EXISTS entities (
	-- The SCOPED id: prefix stem + local part, "customer/C-1042". The stem is the
	-- kind, which is why the id alone is the primary key — a bare "C-1042" that could
	-- be a customer in one source and a container in another is exactly the collision
	-- the prefix exists to prevent.
	id            TEXT PRIMARY KEY,
	namespace_id  TEXT NOT NULL DEFAULT 'default',
	-- "customer" — the prefix stem, stored so it is indexable. DATA: the kernel
	-- validates well-formedness only (non-empty, lowercase snake, no '/'); the
	-- vocabulary is a deployment concern, checked at mapping-confirm time.
	kind          TEXT NOT NULL,
	-- The delivery that first caused this handle to exist. Nullable: an entity minted
	-- by an operator or a background pass has no delivery behind it, and a NOT NULL
	-- here would force a fake one.
	first_seen_evidence TEXT REFERENCES evidence(id),
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_entities_kind ON entities (namespace_id, kind);

CREATE TABLE IF NOT EXISTS links (
	id            TEXT PRIMARY KEY,
	namespace_id  TEXT NOT NULL DEFAULT 'default',
	-- identity | relation | lineage. The three questions a link can answer: is this
	-- the same thing, how does it stand to that thing, what came before it.
	family        TEXT NOT NULL,
	-- Typed refs, so a link can join any two planes without a column per plane:
	-- "entity:customer/C-1042" | "event:<id>" | "decision:<id>" | "evidence:<id>".
	from_ref      TEXT NOT NULL,
	-- For family='identity' the store canonically orders from_ref < to_ref.
	to_ref        TEXT NOT NULL,
	-- The verb. DATA — declared in the RelationRegistry, never branched on.
	relation      TEXT NOT NULL,
	-- candidate | confirmed | retracted. The review lane, not a confidence bucket.
	state         TEXT NOT NULL,
	-- HOW it was asserted: declared | record | reference | shared_object | witnessed |
	-- derived | scored | human | correlation. This column carries the trust ceiling —
	-- a derived/scored/correlation mechanism may never write `confirmed`, which is what
	-- keeps a heuristic from promoting itself into the answer plane.
	mechanism     TEXT NOT NULL,
	-- name@version of the producing pass. The batch revocation key: a producer that
	-- turns out to be wrong is retracted wholesale by this column rather than row by
	-- row, which is the difference between a fixable mistake and a permanent one.
	producer      TEXT NOT NULL DEFAULT '',
	confidence    DOUBLE PRECISION NOT NULL DEFAULT 1.0,
	-- The basis. A link with no evidence and a non-human mechanism is REFUSED by the
	-- store (the admissibility rule): a machine that cannot say why it believes
	-- something has not made an assertion, it has made a guess.
	evidence_id   TEXT REFERENCES evidence(id),
	asserted_by   TEXT NOT NULL,
	asserted_at   TIMESTAMPTZ NOT NULL,
	recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	-- When the described relation HELD, as distinct from when it was asserted — the
	-- bi-temporal pair, so "who owned this in March" survives a later reassignment.
	valid_from    TIMESTAMPTZ,
	valid_to      TIMESTAMPTZ,
	retracted_at  TIMESTAMPTZ,
	-- The source-native reference of whatever produced this link, UNQUALIFIED (no
	-- "@r<rev>" suffix — amendment S1). Deliberate: a mapping-revision bump must
	-- re-derive the same link as a no-op, not duplicate the whole graph the way a
	-- revision-qualified ref once re-archived a whole corpus.
	source_ref    TEXT NOT NULL DEFAULT '',
	-- Replay protection AND the corroboration rule in one key: same producer, same
	-- delivery ⇒ nothing; different mechanism or different source ⇒ a second row.
	CONSTRAINT links_dedup UNIQUE (namespace_id, family, from_ref, to_ref, relation, mechanism, source_ref)
);

-- The two traversal indexes are PARTIAL on the answerable set. A closure walk only ever
-- follows confirmed, unretracted links, so the index that serves it should not carry the
-- candidate backlog — which on a live deployment is the larger half.
CREATE INDEX IF NOT EXISTS idx_links_from ON links (namespace_id, from_ref)
	WHERE state = 'confirmed' AND retracted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_links_to ON links (namespace_id, to_ref)
	WHERE state = 'confirmed' AND retracted_at IS NULL;

-- The review inbox: highest-confidence candidates first, off its own partial index so
-- the operator queue never scans the confirmed graph.
CREATE INDEX IF NOT EXISTS idx_links_review ON links (namespace_id, state, confidence DESC)
	WHERE state = 'candidate';

-- Batch revocation. Not partial: retracting a producer must find its retracted rows too,
-- or a second revocation pass would rescan the table.
CREATE INDEX IF NOT EXISTS idx_links_producer ON links (namespace_id, producer);
