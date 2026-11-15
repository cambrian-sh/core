package config

import (
	"strings"
	"testing"
)

func TestBackupConfig_DefaultsAndValidation(t *testing.T) {
	d := DefaultConfig().Backup
	if d.Dir != "backups" || d.PassphraseEnv != "CAMBRIAN_BACKUP_PASSPHRASE" ||
		d.Retention != (BackupRetention{Daily: 7, Weekly: 4, Monthly: 3}) {
		t.Fatalf("defaults %+v, want backups/ and 7/4/3 (owner decision ENG-681)", d)
	}
	if errs := d.validate("cambrian_db", []string{"cambrian_db"}); len(errs) != 0 {
		t.Fatalf("defaults must validate: %v", errs)
	}
	bad := d
	bad.Retention.Daily = 0
	bad.DrillDBName = "CAMBRIAN_DB"
	bad.PassphraseEnv = " "
	errs := strings.Join(bad.validate("cambrian_db", nil), "\n")
	for _, want := range []string{"backup.retention", "is the live database", "backup.passphrase_env"} {
		if !strings.Contains(errs, want) {
			t.Errorf("validation lacks %q:\n%s", want, errs)
		}
	}
	prot := d
	prot.DrillDBName = "keep_me"
	if errs := prot.validate("cambrian_db", []string{"keep_me"}); len(errs) != 1 {
		t.Fatalf("a protected drill database must be refused: %v", errs)
	}
}
