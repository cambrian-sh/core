package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cambrian-sh/core/internal/backup"
	"github.com/cambrian-sh/core/internal/config"
)

// The `orchestrator backup` subcommand group (T-20261106-decide-the-database-backup-strategy-for,
// ENG-681; owner decision 2026-11-15): backups are DEPLOYMENT-OWNED. The binary takes,
// verifies, rotates, restores and drills them; the host schedules `backup run` nightly
// and `backup drill` weekly (cambrian-core/docs/backup-and-restore.md). It lives in the
// importable app package so the OSS and the premium binary dispatch to the same code.

const backupUsage = `usage:
  orchestrator backup run     [--kind nightly|manual] [--no-secret-key] [--no-prune]
  orchestrator backup verify  [RUN]
  orchestrator backup list
  orchestrator backup prune
  orchestrator backup restore RUN --to-db NAME [--replace-live] [--skip-snapshot]
                                  [--files-to DIR] [--secret-key-to PATH]
  orchestrator backup drill   [RUN] [--keep]

Every verb takes --dir <kernel-dir> (default: the kernel's base directory).
RUN is a run folder path, its name, or "latest" (the default where optional).

run      dumps the database (pg_dump -Fc), copies configs/config.db and the storage
         data directory, encrypts secret.key into backup.key_dir (never next to the
         dump; needs the passphrase in the env var named by backup.passphrase_env),
         writes manifest.json, then applies the 7/4/3 rotation (backup.retention).
verify   checks every file against the manifest, bbolt copies against bbolt's check,
         and that pg_restore can read the dump.
restore  restores the dump into a NEW database (--to-db), checking row counts. Over the
         live database only with --replace-live (a pre-restore snapshot is taken first
         unless --skip-snapshot). --files-to extracts config.db and data/ into a new
         directory; --secret-key-to decrypts secret.key to a path that must not exist.
drill    restores the run into the scratch database backup.drill_dbname, compares every
         table's row count with the manifest and drops it again (--keep keeps it).
`

// RunBackup dispatches `backup …` and returns a process exit code.
func RunBackup(ctx context.Context, args []string) int {
	return runBackup(ctx, args, os.Stdout, os.Stderr)
}

type backupEnv struct {
	cfg   *config.Config
	base  string
	paths backup.Paths
	pg    backup.PG
}

func loadBackupEnv(base string) (*backupEnv, error) {
	cfg, err := config.LoadConfig(filepath.Join(base, "configs", "config.json"))
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	paths, err := backup.ResolvePaths(cfg.Backup, base)
	if err != nil {
		return nil, err
	}
	return &backupEnv{cfg: cfg, base: base, paths: paths, pg: backup.PGFromConfig(cfg)}, nil
}

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, backupUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("backup "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", config.ResolveBaseDir(), "kernel base directory (the one holding configs/)")
	kind := fs.String("kind", backup.KindNightly, "run: nightly | manual")
	noSecret := fs.Bool("no-secret-key", false, "run: do not back up secret.key")
	noPrune := fs.Bool("no-prune", false, "run: skip the retention rotation after the run")
	toDB := fs.String("to-db", "", "restore: target database")
	replaceLive := fs.Bool("replace-live", false, "restore: allow restoring over the live/protected database")
	skipSnapshot := fs.Bool("skip-snapshot", false, "restore --replace-live: do not take the pre-restore snapshot")
	filesTo := fs.String("files-to", "", "restore: extract config.db and data/ into this NEW directory")
	secretTo := fs.String("secret-key-to", "", "restore: decrypt secret.key to this path (must not exist)")
	keep := fs.Bool("keep", false, "drill: keep the scratch database")
	// Interleaved parse: positional RUN may sit before or after flags.
	var pos []string
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	logf := func(format string, a ...any) { fmt.Fprintf(stdout, format+"\n", a...) }

	env, err := loadBackupEnv(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "backup: %v\n", err)
		return 1
	}
	resolveRun := func() (string, error) {
		arg := "latest"
		if len(pos) > 0 {
			arg = pos[0]
		}
		if arg == "latest" {
			return backup.Latest(env.paths.Root, "")
		}
		if st, err := os.Stat(arg); err == nil && st.IsDir() {
			return arg, nil
		}
		p := filepath.Join(env.paths.Root, arg)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("no backup run %q (not a folder, and not under %s)", arg, env.paths.Root)
	}
	fail := func(err error) int { fmt.Fprintf(stderr, "backup %s: %v\n", verb, err); return 1 }

	switch verb {
	case "run":
		if *kind != backup.KindNightly && *kind != backup.KindManual {
			fmt.Fprintf(stderr, "backup run: --kind must be nightly or manual\n")
			return 2
		}
		storePath, off := configStorePath(env.base)
		opts := backup.Options{
			Root: env.paths.Root, KeyRoot: env.paths.KeyRoot, Kind: *kind, PG: env.pg,
			DataDir: absUnder(env.base, env.cfg.Storage.DataDir), SkipSecretKey: *noSecret,
			PassphraseEnv: env.cfg.Backup.PassphraseEnv, Passphrase: []byte(os.Getenv(env.cfg.Backup.PassphraseEnv)),
			Exclude: []string{env.paths.Root, env.paths.KeyRoot}, Binary: binaryName(), Log: logf,
		}
		if !off {
			opts.ConfigStore = storePath
			// storage.OpenConfigStore keeps the key beside the store (ADR-0101 D6).
			opts.SecretKeyPath = filepath.Join(filepath.Dir(storePath), "secret.key")
		}
		runDir, m, err := backup.Run(ctx, opts)
		if err != nil {
			if runDir != "" {
				_ = os.RemoveAll(runDir) // never leave a half run that looks like a backup
			}
			return fail(err)
		}
		logf("backup run %s complete: %d files, %d tables (%d rows) in %s", m.Kind, len(m.Files), len(m.Tables), totalRows(m.Tables), runDir)
		if !*noPrune {
			removed, err := backup.Prune(env.paths.Root, env.paths.KeyRoot, backup.RetentionFromConfig(env.cfg.Backup), time.Now())
			if err != nil {
				return fail(fmt.Errorf("rotation: %w", err))
			}
			for _, r := range removed {
				logf("rotated out %s", r)
			}
		}
		return 0

	case "verify":
		runDir, err := resolveRun()
		if err != nil {
			return fail(err)
		}
		m, err := backup.Verify(ctx, runDir, &env.pg)
		if err != nil {
			return fail(err)
		}
		logf("%s verified: %d files, database %s at schema version %d, %d tables", runDir, len(m.Files), m.Database, m.SchemaVersion, len(m.Tables))
		return 0

	case "list":
		runs, err := backup.ListRuns(env.paths.Root)
		if err != nil {
			return fail(err)
		}
		keepSet := backup.Keep(runs, backup.RetentionFromConfig(env.cfg.Backup), time.Now())
		fmt.Fprintf(stdout, "%-36s %-14s %-9s %s\n", "RUN", "KIND", "COMPLETE", "ROTATION")
		for _, r := range runs {
			rot := "kept"
			if !keepSet[r.Name] {
				rot = "next prune removes"
			}
			fmt.Fprintf(stdout, "%-36s %-14s %-9v %s\n", r.Name, r.Kind, r.Complete, rot)
		}
		return 0

	case "prune":
		removed, err := backup.Prune(env.paths.Root, env.paths.KeyRoot, backup.RetentionFromConfig(env.cfg.Backup), time.Now())
		if err != nil {
			return fail(err)
		}
		logf("pruned %d run(s): %s", len(removed), strings.Join(removed, ", "))
		return 0

	case "restore":
		runDir, err := resolveRun()
		if err != nil {
			return fail(err)
		}
		if *toDB == "" && *filesTo == "" && *secretTo == "" {
			fmt.Fprint(stderr, "backup restore: nothing to restore — give --to-db, --files-to and/or --secret-key-to\n")
			return 2
		}
		if *toDB != "" {
			ro := backup.RestoreOptions{Live: env.cfg.Database.DBName, Protected: env.cfg.Database.Migrations.ProtectedDBNames,
				ReplaceLive: *replaceLive, Logf: logf}
			if *replaceLive && strings.EqualFold(*toDB, env.cfg.Database.DBName) && !*skipSnapshot {
				snap, err := backup.SnapshotBeforeRestore(ctx, env.cfg, env.paths, "before restoring "+filepath.Base(runDir)+" over the live database", nil)
				if err != nil {
					return fail(fmt.Errorf("pre-restore snapshot failed (nothing was restored; --skip-snapshot overrides): %w", err))
				}
				logf("pre-restore snapshot: %s", snap)
			}
			_, mism, err := backup.RestoreDatabase(ctx, runDir, env.pg, *toDB, ro)
			if err != nil {
				return fail(err)
			}
			if len(mism) > 0 {
				for _, x := range mism {
					fmt.Fprintln(stderr, "  "+x.String())
				}
				return fail(fmt.Errorf("restored into %s, but %d table row count(s) differ from the manifest", *toDB, len(mism)))
			}
			logf("restored %s into database %s; every table's row count matches the manifest", filepath.Base(runDir), *toDB)
		}
		m, err := backup.ReadManifest(runDir)
		if err != nil {
			return fail(err)
		}
		if *filesTo != "" {
			if _, err := backup.Verify(ctx, runDir, nil); err != nil {
				return fail(err)
			}
			n, err := backup.ExtractFiles(runDir, m, *filesTo)
			if err != nil {
				return fail(err)
			}
			logf("extracted %d file(s) into %s — stop the kernel, then move config.db and data/ into place", n, *filesTo)
		}
		if *secretTo != "" {
			if m.SecretKey == nil || m.SecretKey.Status != "encrypted" {
				return fail(errors.New("this run holds no encrypted secret.key"))
			}
			pass := os.Getenv(env.cfg.Backup.PassphraseEnv)
			if pass == "" {
				return fail(fmt.Errorf("set %s to the backup passphrase", env.cfg.Backup.PassphraseEnv))
			}
			if err := backup.DecryptFile(m.SecretKey.Path, *secretTo, []byte(pass)); err != nil {
				return fail(err)
			}
			logf("secret.key decrypted to %s", *secretTo)
		}
		return 0

	case "drill":
		runDir, err := resolveRun()
		if err != nil {
			return fail(err)
		}
		scratch := backup.DrillDBName(env.cfg)
		ro := backup.RestoreOptions{Live: env.cfg.Database.DBName, Protected: env.cfg.Database.Migrations.ProtectedDBNames, Logf: logf}
		res, err := backup.Drill(ctx, runDir, env.pg, scratch, ro, *keep)
		if err != nil {
			return fail(err)
		}
		if len(res.Mismatches) > 0 {
			for _, x := range res.Mismatches {
				fmt.Fprintln(stderr, "  "+x.String())
			}
			return fail(fmt.Errorf("restore drill of %s FAILED: %d table row count(s) differ", res.Run, len(res.Mismatches)))
		}
		logf("restore drill of %s passed: %d tables, %d rows restored into %s and matched the manifest (scratch database %s)",
			res.Run, res.Tables, res.Rows, scratch, map[bool]string{true: "kept", false: "dropped"}[res.Kept])
		return 0

	default:
		fmt.Fprintf(stderr, "unknown backup subcommand %q\n%s", verb, backupUsage)
		return 2
	}
}

func absUnder(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func totalRows(t []backup.TableCount) int64 {
	var n int64
	for _, x := range t {
		n += x.Rows
	}
	return n
}

func binaryName() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Base(exe)
}
