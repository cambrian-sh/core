-- 0030_memory_notes.sql — ADR-0139 D14 principal memory notes.
-- ADDITIVE: three new tables; nothing is changed or dropped, and older binaries ignore them.
-- (0029 is left for the ADR-0139 D13 skill-lifecycle tables, landing in parallel.)
--
-- A notes document is one bounded, mutable markdown document per (scope, owner):
-- scope 'personal' (owner = a principal id) or 'org' (owner = an org id). They live in
-- their own tables rather than the vector memory store because they are never searched,
-- embedded or ranked — they are addressed by key and edited under an etag
-- compare-and-swap (internal/memorynotes, decision logged on
-- T-20261105-adr-0139-d14-principal-memory-notes-bounded).
--
-- memory_notes: `revision` counts every write INCLUDING a delete. A delete keeps the row
-- (deleted = true, content = '') so the revision sequence never restarts and the history
-- key (scope, owner_id, revision) stays deterministic (AGENTS.md §2.6): a retried write
-- lands on the same key instead of a duplicate row.
--
-- memory_notes_budgets: operator budgets in Unicode code points. owner_id '' is the
-- scope default; a row with an owner is that document's own budget. Absent rows fall
-- back to execution.memory_notes.{personal,org}_budget_chars. Lowering a budget never
-- rewrites content: run snapshots are then served cut and flagged truncated.
--
-- memory_notes_history: one row per write — who, why, which op, and the document AFTER
-- the write (for a delete, the text that was cleared, so a delete is recoverable).
--
-- Rollback: DROP TABLE memory_notes_history, memory_notes_budgets, memory_notes;

CREATE TABLE IF NOT EXISTS memory_notes (
	scope      TEXT NOT NULL CHECK (scope IN ('personal', 'org')),
	owner_id   TEXT NOT NULL CHECK (owner_id <> ''),
	content    TEXT NOT NULL DEFAULT '',
	revision   BIGINT NOT NULL CHECK (revision > 0),
	deleted    BOOLEAN NOT NULL DEFAULT false,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_by TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (scope, owner_id)
);

CREATE TABLE IF NOT EXISTS memory_notes_budgets (
	scope        TEXT NOT NULL CHECK (scope IN ('personal', 'org')),
	owner_id     TEXT NOT NULL DEFAULT '',
	budget_chars INT NOT NULL CHECK (budget_chars > 0),
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (scope, owner_id)
);

CREATE TABLE IF NOT EXISTS memory_notes_history (
	scope    TEXT NOT NULL,
	owner_id TEXT NOT NULL,
	revision BIGINT NOT NULL,
	op       TEXT NOT NULL,
	actor    TEXT NOT NULL DEFAULT '',
	reason   TEXT NOT NULL DEFAULT '',
	content  TEXT NOT NULL DEFAULT '',
	at       TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (scope, owner_id, revision)
);
