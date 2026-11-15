package config

import (
	"fmt"
	"strings"
)

// BackupConfig is the deployment-owned backup of the kernel's durable state, config
// block `backup` (T-20261106-decide-the-database-backup-strategy-for, ENG-681; owner
// decision 2026-11-15). It is read by internal/backup through `orchestrator backup …`
// and by the mandatory pre-destructive-migration snapshot (internal/migrate Policy.Snapshot).
//
// Scope of a full run: the Postgres database (pg_dump -Fc), the ADR-0101 config store
// (configs/config.db), the storage data directory (agents.db reactive journal, content
// store, vault, outputs) and secret.key — the last ENCRYPTED, in KeyDir, never next to a dump.
type BackupConfig struct {
	// Dir is the root of the timestamped backup folders. Relative paths resolve against
	// the kernel's base directory. Default "backups".
	Dir string `json:"dir"`
	// KeyDir holds the encrypted secret.key copies, one folder per run, kept apart from
	// the dumps. Empty = a sibling of Dir named "<Dir>-keys".
	KeyDir string `json:"key_dir"`
	// PassphraseEnv names the environment variable holding the passphrase secret.key is
	// encrypted with (the passphrase itself is never a config value). Default
	// CAMBRIAN_BACKUP_PASSPHRASE.
	PassphraseEnv string `json:"passphrase_env"`
	// PGContainer, when set, runs pg_dump / pg_restore inside this Docker container over
	// its local socket (no PostgreSQL client tools needed on the host). Empty = pg_dump /
	// pg_restore from PATH, connecting with the database block's host/port/user.
	PGContainer string `json:"pg_container"`
	// DrillDBName is the scratch database the weekly restore drill restores into on the
	// same server. Empty = "<database.dbname>_restore_drill". It may never be the live
	// database or a protected one.
	DrillDBName string `json:"drill_dbname"`
	// Retention is the grandfather-father-son rotation applied after every run.
	Retention BackupRetention `json:"retention"`
}

// BackupRetention keeps the newest run of each of the last Daily days, Weekly ISO weeks
// and Monthly months (a run may satisfy several). Pre-migration and pre-restore
// snapshots are never rotated away automatically.
type BackupRetention struct {
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

// DefaultBackupConfig is the owner-decided default: backups/ next to the config
// bundle, 7 daily / 4 weekly / 3 monthly.
func DefaultBackupConfig() BackupConfig {
	return BackupConfig{
		Dir:           "backups",
		PassphraseEnv: "CAMBRIAN_BACKUP_PASSPHRASE",
		Retention:     BackupRetention{Daily: 7, Weekly: 4, Monthly: 3},
	}
}

// drillDBName is DrillDBName or its default for the live database name, for validation;
// internal/backup.DrillDBName is the same rule for the drill itself.
func (b BackupConfig) drillDBName(live string) string {
	if strings.TrimSpace(b.DrillDBName) != "" {
		return strings.TrimSpace(b.DrillDBName)
	}
	return live + "_restore_drill"
}

func (b BackupConfig) validate(liveDB string, protected []string) []string {
	var errs []string
	if strings.TrimSpace(b.Dir) == "" {
		errs = append(errs, "backup.dir must not be empty")
	}
	if strings.TrimSpace(b.PassphraseEnv) == "" {
		errs = append(errs, "backup.passphrase_env must name an environment variable")
	}
	r := b.Retention
	if r.Daily < 1 || r.Weekly < 0 || r.Monthly < 0 {
		errs = append(errs, fmt.Sprintf("backup.retention must keep at least 1 daily run and no negative counts (got daily=%d weekly=%d monthly=%d)",
			r.Daily, r.Weekly, r.Monthly))
	}
	if liveDB != "" {
		drill := b.drillDBName(liveDB)
		if strings.EqualFold(drill, liveDB) {
			errs = append(errs, fmt.Sprintf("backup.drill_dbname %q is the live database; the restore drill must use a scratch database", drill))
		}
		for _, p := range protected {
			if strings.EqualFold(strings.TrimSpace(p), drill) {
				errs = append(errs, fmt.Sprintf("backup.drill_dbname %q is listed in database.migrations.protected_dbnames", drill))
			}
		}
	}
	return errs
}
