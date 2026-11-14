package migrate

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Tests for 0047_substrate_lane_stream_keys.sql (owner decision 2026-11-14,
// ENG-1068): substrate-lane typed rows written before the fix are re-keyed
// `<stream>:<source_ref>`.

const substrateLaneKeysVersion int64 = 47

func TestLoadMigrations_Includes0047SubstrateLaneStreamKeys(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != substrateLaneKeysVersion {
			continue
		}
		if m.name != "substrate_lane_stream_keys" {
			t.Fatalf("migration 47 is %q", m.name)
		}
		for _, s := range []string{
			"UPDATE events e SET source_ref = chr(1) || k.new_ref",
			"UPDATE observations o SET source_ref = k.new_ref",
			"ev.source_revision = ''",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0047 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if !strings.HasPrefix(l, "--") && (strings.Contains(l, "DROP TABLE") || strings.Contains(l, "DROP INDEX") || strings.Contains(l, "ALTER ") ||
				strings.Contains(l, "DELETE FROM EVENTS") || strings.Contains(l, "DELETE FROM OBSERVATIONS")) {
				t.Errorf("0047 must only re-key rows: %q", line)
			}
		}
		return
	}
	t.Fatal("0047_substrate_lane_stream_keys is not embedded")
}

// Two streams that both sent ref "42" before the fix (only one row survived the
// old collision, the other stream used "43"), a post-fix revisioned row, a
// revision-less row the fixed lane already keyed, a non-substrate row, and a row
// whose new key would collide with an untouched row: after 0047 the pre-fix rows
// carry `<stream>:<ref>`, everything else is unchanged. Postgres-backed
// (splitTestPool skips without a DSN); the rows live in a namespace of their own
// and are deleted afterwards.
func TestSubstrateLaneStreamKeys_RekeysPreFixRowsOnly(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	t.Cleanup(func() { freshSchema(ctx, t, pool) })
	applyThrough(ctx, t, pool, substrateLaneKeysVersion-1)

	ns := fmt.Sprintf("pgtest-eng1068-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM observations WHERE namespace_id = $1`,
			`DELETE FROM events WHERE namespace_id = $1`,
			`DELETE FROM evidence WHERE namespace_id = $1`,
		} {
			if _, err := pool.Exec(ctx, q, ns); err != nil {
				t.Logf("cleanup: %v", err)
			}
		}
	})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	evidence := func(id, source, revision string) {
		exec(`INSERT INTO evidence (id, namespace_id, source_id, source_key, source_revision, content_hash)
		      VALUES ($1, $2, $3, $1, $4, 'sha256:test')`, ns+id, ns, source, revision)
	}
	event := func(id, ev, ref string) {
		exec(`INSERT INTO events (id, namespace_id, event_type, occurred_at, evidence_id, source_ref)
		      VALUES ($1, $2, 'pgtest', now(), $3, $4)`, ns+id, ns, ns+ev, ref)
	}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	observe := func(ev, ref string) {
		exec(`INSERT INTO observations (namespace_id, entity_id, predicate, value_type, value_text, occurred_at, evidence_id, source_ref)
		      VALUES ($1, 'pgtest/e', 'p', 'text', 'v', $2, $3, $4)`, ns, at, ns+ev, ref)
	}

	evidence("-a", "event:orders", "")        // pre-fix, stream orders
	evidence("-b", "event:shipments", "")     // pre-fix, stream shipments
	evidence("-new", "event:orders", "r1")    // post-fix, revisioned
	evidence("-done", "event:orders", "")     // revision-less, already keyed by the fixed lane
	evidence("-ing", "ingress:crm", "")       // not the substrate lane
	evidence("-clash", "event:shipments", "") // its new key would collide

	event("-e1", "-a", "42")
	observe("-a", "42#obs0")
	event("-e2", "-b", "43")
	event("-e3", "-new", "orders:44#r1")
	event("-e4", "-done", "orders:45")
	event("-e5", "-ing", "46")
	event("-e6", "-ing", "shipments:47") // an untouched row already holding the key
	event("-e7", "-clash", "47")         // would become shipments:47

	applyThrough(ctx, t, pool, substrateLaneKeysVersion)

	want := map[string]string{
		"-e1": "orders:42",
		"-e2": "shipments:43",
		"-e3": "orders:44#r1",
		"-e4": "orders:45",
		"-e5": "46",
		"-e6": "shipments:47",
		"-e7": "47", // left alone rather than failing the boot
	}
	for id, ref := range want {
		var got string
		if err := pool.QueryRow(ctx, `SELECT source_ref FROM events WHERE id = $1`, ns+id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got != ref {
			t.Errorf("event %s source_ref = %q, want %q", id, got, ref)
		}
	}
	var obs string
	if err := pool.QueryRow(ctx, `SELECT source_ref FROM observations WHERE namespace_id = $1`, ns).Scan(&obs); err != nil {
		t.Fatalf("read observation: %v", err)
	}
	if obs != "orders:42#obs0" {
		t.Errorf("observation source_ref = %q, want orders:42#obs0", obs)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM events WHERE namespace_id = $1 AND source_ref LIKE chr(1) || '%'`, ns); n != 0 {
		t.Errorf("%d rows kept the sentinel prefix", n)
	}
}
