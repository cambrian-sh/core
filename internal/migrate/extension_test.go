package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnsureVectorExtension (extension.go, ENG-2061) on scratch databases that start
// without pgvector, as a fresh Postgres server's database does.

func hasVector(ctx context.Context, t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestPg_EnsureVectorExtension_CreatesWhenPermitted(t *testing.T) {
	ctx, open, _ := scratchDB(t)
	pool := open()
	if hasVector(ctx, t, pool) {
		t.Fatal("a fresh scratch database must start without pgvector")
	}
	cc := pool.Config().ConnConfig
	if err := EnsureVectorExtension(ctx, cc, true, Policy{}); err != nil {
		t.Fatalf("auto mode on an unprotected database must create the extension: %v", err)
	}
	if !hasVector(ctx, t, pool) {
		t.Fatal("the extension was not created")
	}
	// Idempotent: a second boot finds it and writes nothing.
	if err := EnsureVectorExtension(ctx, cc, true, Policy{}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !lockFree(ctx, t, pool) {
		t.Fatal("the migration lock must be released")
	}
}

func TestPg_EnsureVectorExtension_RefusesWhenTheBootMayNotChangeTheSchema(t *testing.T) {
	ctx, open, name := scratchDB(t)
	pool := open()
	cc := pool.Config().ConnConfig
	cases := []struct {
		label       string
		autoMigrate bool
		p           Policy
		reason      string
	}{
		{"auto_migrate off", false, Policy{}, "runner is off"},
		{"mode off", true, Policy{Mode: ModeOff}, "runner is off"},
		{"protected", true, Policy{ProtectedDBNames: []string{strings.ToUpper(name)}}, "protected_dbnames"},
		{"refuse_pending", true, Policy{Mode: ModeRefusePending}, "refuse_pending"},
	}
	for _, c := range cases {
		err := EnsureVectorExtension(ctx, cc, c.autoMigrate, c.p)
		if !errors.Is(err, ErrVectorExtensionMissing) {
			t.Fatalf("%s: want ErrVectorExtensionMissing, got %v", c.label, err)
		}
		for _, want := range []string{VectorExtensionSQL, c.reason, name, "Nothing was written"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal must name %q: %v", c.label, want, err)
			}
		}
	}
	if hasVector(ctx, t, pool) {
		t.Fatal("a refused boot must not create the extension")
	}
	// The explicit opt-in lets a guarded boot create it, as it lets it apply the baseline.
	if err := EnsureVectorExtension(ctx, cc, true, Policy{ProtectedDBNames: []string{name}, AllowPending: true}); err != nil {
		t.Fatalf("protected + allow_pending must create it: %v", err)
	}
	if !hasVector(ctx, t, pool) {
		t.Fatal("allow_pending did not create the extension")
	}
}

// A role without CREATE on the database cannot create the extension: the error says
// so and names the SQL to run as a privileged role.
func TestPg_EnsureVectorExtension_InsufficientPrivilegeNamesTheSQL(t *testing.T) {
	ctx, open, _ := scratchDB(t)
	pool := open()
	role := fmt.Sprintf("q_noext_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'pw'`, role)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role) // best effort: scratch role
	})
	cc := pool.Config().ConnConfig.Copy()
	cc.User, cc.Password = role, "pw"
	err := EnsureVectorExtension(ctx, cc, true, Policy{})
	if !errors.Is(err, ErrVectorExtensionMissing) {
		t.Fatalf("want ErrVectorExtensionMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), "may not create extensions") || !strings.Contains(err.Error(), VectorExtensionSQL) {
		t.Fatalf("the error must explain the privilege and name the SQL: %v", err)
	}
	if hasVector(ctx, t, pool) {
		t.Fatal("nothing must be created")
	}
	if !lockFree(ctx, t, pool) {
		t.Fatal("the migration lock must be released after a failed create")
	}
}

// Several binaries booting on one fresh database at once all succeed: the lock
// serialises CREATE EXTENSION, which can otherwise collide on pg_extension's index.
func TestPg_EnsureVectorExtension_ConcurrentBoots(t *testing.T) {
	ctx, open, _ := scratchDB(t)
	pool := open()
	cc := pool.Config().ConnConfig
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = EnsureVectorExtension(ctx, cc, true, Policy{})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("boot %d: %v", i, err)
		}
	}
	if !hasVector(ctx, t, pool) {
		t.Fatal("the extension was not created")
	}
}

func TestEnsureVectorExtension_UnreachableIsAConnectError(t *testing.T) {
	cc, err := pgx.ParseConfig("host=127.0.0.1 port=1 user=x password=x dbname=x connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	err = EnsureVectorExtension(context.Background(), cc, true, Policy{})
	if err == nil || errors.Is(err, ErrVectorExtensionMissing) || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("an unreachable server is a connection error, not a missing extension: %v", err)
	}
}
