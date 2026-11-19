package postgres

import (
	"os"
	"strings"
	"testing"
)

// ENG-1103 guard: RecordObservation takes the entity's object lock inside its
// INSERT (the insert reads FROM the lock CTE) and calls the locked maintainer;
// the unlocked ApplyStateTx there would take the lock a second time, and an
// insert without the CTE would skip it.
func TestRecordObservation_LocksInsideTheInsert(t *testing.T) {
	src, err := os.ReadFile("event_store.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func (s *PgEventStore) RecordObservation(")
	if i < 0 {
		t.Fatal("RecordObservation not found")
	}
	body := s[i:]
	body = body[:strings.Index(body, "\n}\n")]
	for _, want := range []string{`objects.LockedInsertCTE("$17")`, "FROM l", "objects.EntityLockKey(ns, ob.EntityID)", "objects.ApplyLockedStateTx("} {
		if !strings.Contains(body, want) {
			t.Errorf("RecordObservation lacks %s", want)
		}
	}
	if strings.Contains(body, "objects.ApplyStateTx(") {
		t.Error("RecordObservation must use ApplyLockedStateTx (its insert holds the lock)")
	}
}
