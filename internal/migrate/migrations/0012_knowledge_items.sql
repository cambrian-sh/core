-- 0012_knowledge_items.sql — the epistemic split's interpretation + belief layers
-- (ADR-0106, Knowledge Substrate Phase 2a).
--
-- knowledge_items are APPEND-ONLY typed interpretations. Two contradictory items
-- about the same entity and predicate are BOTH valid records of what different
-- sources said — there is deliberately NO uniqueness on (entity, predicate,
-- validity): a global non-overlap constraint would make contradiction
-- unrepresentable, and the disagreement is the signal (memo §8, the v2 bug).
--
-- resolutions are the DERIVED, versioned belief: one current row per
-- (namespace, kind, entity, actor, policy), recomputed from the FULL applicable
-- item set on every write so the outcome cannot depend on arrival order
-- (memo §13). Non-overlap appears ONLY here — the resolved projection is the one
-- place "one current belief" is the defining property.
--
-- ADDITIVE. Rollback: DROP TABLE resolutions; DROP TABLE statement_values;
-- DROP TABLE knowledge_items;

CREATE TABLE IF NOT EXISTS knowledge_items (
	id             TEXT PRIMARY KEY,
	namespace_id   TEXT NOT NULL DEFAULT 'default',
	-- What kind of interpretation this is ("commitment", …). Kinds are DATA:
	-- the kernel never branches on a specific kind (no-benchmark-logic /
	-- generic-naming rules both hold).
	kind           TEXT NOT NULL,
	-- The evidence this interpretation derives from. NULLABLE in Phase 2a
	-- because the first producer's lane cannot see evidence ids yet (DECISIONS
	-- 2026-08-01 D-D); Phase 2b populates it and tightens the contract.
	evidence_id    TEXT REFERENCES evidence(id),
	-- Source-scoped entity the item is about ("purchase_order/PO-4471").
	entity_id      TEXT NOT NULL,
	-- The attributed actor who asserted it, and when THEY asserted it (memo §8
	-- asserted_at — distinct from when we stored it).
	asserted_by    TEXT NOT NULL DEFAULT '',
	asserted_at    TIMESTAMPTZ NOT NULL,
	-- Source-native reference of the assertion (message id). With entity+kind it
	-- is the idempotency key: the same assertion replayed creates nothing.
	source_ref     TEXT NOT NULL DEFAULT '',
	-- negation=true retires the actor's prior assertion about this entity
	-- without replacing it. A retraction is itself an item — recorded, never a
	-- deletion (memo §16: never destructively merge or erase interpretations).
	negation       BOOLEAN NOT NULL DEFAULT false,
	classification TEXT[] NOT NULL DEFAULT '{}',
	valid_from     TIMESTAMPTZ,
	valid_to       TIMESTAMPTZ,
	schema_version INT NOT NULL DEFAULT 1,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotency, not overlap-prevention: one row per (kind, entity, source_ref)
-- inside a namespace, and only when a source_ref exists at all.
CREATE UNIQUE INDEX IF NOT EXISTS knowledge_items_source_ref_uniq
	ON knowledge_items (namespace_id, kind, entity_id, source_ref)
	WHERE source_ref <> '';

CREATE INDEX IF NOT EXISTS idx_knowledge_items_entity
	ON knowledge_items (namespace_id, kind, entity_id, asserted_by);

-- Typed values, exactly one column per row non-null (memo §6: the narrow
-- envelope + typed statement_values is what keeps the deferred Event/Observation
-- split additive; a JSONB payload here would quietly become the God Object).
CREATE TABLE IF NOT EXISTS statement_values (
	item_id         TEXT NOT NULL REFERENCES knowledge_items(id) ON DELETE CASCADE,
	predicate       TEXT NOT NULL,
	value_type      TEXT NOT NULL,
	value_date      TIMESTAMPTZ,
	value_number    DOUBLE PRECISION,
	value_text      TEXT,
	value_entity_id TEXT,
	PRIMARY KEY (item_id, predicate),
	CONSTRAINT statement_values_exactly_one
		CHECK (num_nonnulls(value_date, value_number, value_text, value_entity_id) = 1)
);

CREATE TABLE IF NOT EXISTS resolutions (
	id           BIGSERIAL PRIMARY KEY,
	namespace_id TEXT NOT NULL DEFAULT 'default',
	kind         TEXT NOT NULL,
	entity_id    TEXT NOT NULL,
	actor        TEXT NOT NULL DEFAULT '',
	-- The authority policy that produced this belief. Phase 2a ships exactly
	-- one deterministic policy ("latest_assertion"); the column exists so a
	-- second policy is a new row family, not a migration.
	policy       TEXT NOT NULL,
	-- The winning item, or NULL when the actor's latest word was a negation.
	item_id      TEXT REFERENCES knowledge_items(id),
	reason_code  TEXT NOT NULL DEFAULT '',
	valid_from   TIMESTAMPTZ,
	-- System-time versioning: the current version has system_to NULL; a
	-- recompute that changes the answer CLOSES the old version rather than
	-- rewriting it, so "what did we believe on date X" stays answerable.
	system_from  TIMESTAMPTZ NOT NULL DEFAULT now(),
	system_to    TIMESTAMPTZ
);

-- THE resolved-projection constraint: at most one CURRENT belief per key.
-- This is the only place in the substrate where non-overlap is allowed.
CREATE UNIQUE INDEX IF NOT EXISTS resolutions_current_uniq
	ON resolutions (namespace_id, kind, entity_id, actor, policy)
	WHERE system_to IS NULL;

CREATE INDEX IF NOT EXISTS idx_resolutions_current_kind
	ON resolutions (namespace_id, kind, policy)
	WHERE system_to IS NULL;
