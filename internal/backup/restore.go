package backup

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// DrillMarker is the database comment the restore drill puts on its scratch database. The
// drill drops an existing scratch database only when it carries this comment, so a
// mis-set backup.drill_dbname can never drop a database the drill did not create.
const DrillMarker = "cambrian backup restore-drill scratch database (safe to drop)"

// CountMismatch is one table whose restored row count differs from the manifest.
type CountMismatch struct {
	Table         string
	Want, Got     int64
	MissingInCopy bool
	MissingInDump bool
}

func (m CountMismatch) String() string {
	switch {
	case m.MissingInCopy:
		return fmt.Sprintf("%s: in the manifest (%d rows) but not in the restored database", m.Table, m.Want)
	case m.MissingInDump:
		return fmt.Sprintf("%s: in the restored database (%d rows) but not in the manifest", m.Table, m.Got)
	default:
		return fmt.Sprintf("%s: manifest %d rows, restored %d", m.Table, m.Want, m.Got)
	}
}

// CompareCounts compares restored row counts with the manifest's.
func CompareCounts(want, got []TableCount) []CountMismatch {
	g := map[string]int64{}
	for _, t := range got {
		g[t.Table] = t.Rows
	}
	w := map[string]bool{}
	var out []CountMismatch
	for _, t := range want {
		w[t.Table] = true
		rows, ok := g[t.Table]
		switch {
		case !ok:
			out = append(out, CountMismatch{Table: t.Table, Want: t.Rows, MissingInCopy: true})
		case rows != t.Rows:
			out = append(out, CountMismatch{Table: t.Table, Want: t.Rows, Got: rows})
		}
	}
	for _, t := range got {
		if !w[t.Table] {
			out = append(out, CountMismatch{Table: t.Table, Got: t.Rows, MissingInDump: true})
		}
	}
	return out
}

// RestoreOptions configure RestoreDatabase.
type RestoreOptions struct {
	// Live is the kernel's own database name (database.dbname).
	Live string
	// Protected are database.migrations.protected_dbnames.
	Protected []string
	// ReplaceLive allows restoring over Live (or a protected database) — the
	// disaster-recovery path. Without it a restore only creates a NEW database.
	ReplaceLive bool
}

// RestoreDatabase loads a verified run's dump into target on pg's server and checks the
// restored row counts against the manifest. A target that does not exist is created; an
// existing one is refused unless it is the live/protected database and ReplaceLive is
// set, in which case the dump's objects replace the existing ones (pg_restore --clean).
// The caller takes a pre-restore snapshot of a live target first.
func RestoreDatabase(ctx context.Context, dir string, pg PG, target string, o RestoreOptions) (*Manifest, []CountMismatch, error) {
	m, err := Verify(ctx, dir, &pg)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(target) == "" {
		return nil, nil, fmt.Errorf("restore: no target database given (--to-db)")
	}
	guarded := strings.EqualFold(target, o.Live)
	for _, p := range o.Protected {
		if strings.EqualFold(strings.TrimSpace(p), target) {
			guarded = true
		}
	}
	if guarded && !o.ReplaceLive {
		return nil, nil, fmt.Errorf("restore: %q is the live or a protected database; restoring over it replaces its "+
			"contents — pass --replace-live (a pre-restore snapshot is taken first) or restore into a new database", target)
	}
	exists, _, err := pg.databaseExists(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	t := pg.WithDB(target)
	switch {
	case exists && !guarded:
		return nil, nil, fmt.Errorf("restore: database %q already exists; restore into a new database name "+
			"(a restore never replaces a database other than the live one, and that only with --replace-live)", target)
	case exists:
		if err := t.restoreArchive(ctx, filepath.Join(dir, "db.dump"), true); err != nil {
			return nil, nil, err
		}
	default:
		if err := pg.createDatabase(ctx, target, ""); err != nil {
			return nil, nil, err
		}
		if err := t.restoreArchive(ctx, filepath.Join(dir, "db.dump"), false); err != nil {
			return nil, nil, err
		}
	}
	got, err := CountTables(ctx, t)
	if err != nil {
		return m, nil, err
	}
	return m, CompareCounts(m.Tables, got), nil
}

// DrillResult is the outcome of one restore drill.
type DrillResult struct {
	Run        string
	ScratchDB  string
	Tables     int
	Rows       int64
	Mismatches []CountMismatch
	Kept       bool
}

// Drill verifies a run and restores its dump into a scratch database on pg's server,
// compares every table's row count with the manifest, then drops the scratch database
// (unless keep). The scratch database may never be the live or a protected one, and an
// existing scratch database is dropped only when it carries DrillMarker.
func Drill(ctx context.Context, dir string, pg PG, scratch string, o RestoreOptions, keep bool) (*DrillResult, error) {
	if strings.TrimSpace(scratch) == "" {
		return nil, fmt.Errorf("drill: no scratch database name")
	}
	if strings.EqualFold(scratch, o.Live) {
		return nil, fmt.Errorf("drill: scratch database %q is the live database; refusing", scratch)
	}
	for _, p := range o.Protected {
		if strings.EqualFold(strings.TrimSpace(p), scratch) {
			return nil, fmt.Errorf("drill: scratch database %q is protected; refusing", scratch)
		}
	}
	m, err := Verify(ctx, dir, &pg)
	if err != nil {
		return nil, err
	}
	exists, comment, err := pg.databaseExists(ctx, scratch)
	if err != nil {
		return nil, err
	}
	if exists {
		if comment != DrillMarker {
			return nil, fmt.Errorf("drill: database %q exists and was not created by the drill (no drill marker); "+
				"refusing to drop it — choose another backup.drill_dbname", scratch)
		}
		if err := pg.dropDatabase(ctx, scratch); err != nil {
			return nil, err
		}
	}
	if err := pg.createDatabase(ctx, scratch, DrillMarker); err != nil {
		return nil, err
	}
	res := &DrillResult{Run: filepath.Base(dir), ScratchDB: scratch, Kept: keep}
	t := pg.WithDB(scratch)
	restoreErr := t.restoreArchive(ctx, filepath.Join(dir, "db.dump"), false)
	var got []TableCount
	if restoreErr == nil {
		got, restoreErr = CountTables(ctx, t)
	}
	if !keep {
		if err := pg.dropDatabase(ctx, scratch); err != nil && restoreErr == nil {
			restoreErr = fmt.Errorf("drill passed the restore but could not drop %s: %w", scratch, err)
		}
	}
	if restoreErr != nil {
		return res, restoreErr
	}
	res.Mismatches = CompareCounts(m.Tables, got)
	for _, tc := range m.Tables {
		res.Tables++
		res.Rows += tc.Rows
	}
	return res, nil
}
