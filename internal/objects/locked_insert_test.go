package objects

import (
	"fmt"
	"strings"
	"testing"
)

// ENG-1103: the CTE takes the entity's transaction lock with the same key every
// object-row writer uses, and the insert must read FROM it.
func TestLockedInsertCTE(t *testing.T) {
	want := "WITH l AS (SELECT pg_advisory_xact_lock_shared(" + fmt.Sprint(declarationsLockKey) + "), pg_advisory_xact_lock($17::bigint))"
	if got := LockedInsertCTE("$17"); got != want {
		t.Fatalf("cte = %q", got)
	}
	if EntityLockKey("default", "order/1") != entityLockKey("default", "order/1") ||
		EntityLockKey("default", "order/1") == EntityLockKey("default", "order/2") {
		t.Fatal("EntityLockKey must be the maintainer's key")
	}
	if !strings.Contains(LockedInsertCTE("$1"), "pg_advisory_xact_lock") {
		t.Fatal("a transaction-scoped lock (held until commit)")
	}
}
