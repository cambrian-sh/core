package backup

import (
	"context"
	"slices"
	"strings"
	"testing"
)

const mb = int64(1 << 20)

func TestDecideIndexBuild(t *testing.T) {
	cases := []struct {
		name     string
		shm, mwm int64
		workers  int
		serial   bool
		mention  string
	}{
		{"docker default 64 MB shm, 64 MB maintenance_work_mem", 63 * mb, 64 * mb, 2, true, "shm_size"},
		{"shm just under twice maintenance_work_mem", 127 * mb, 64 * mb, 2, true, "shm_size"},
		{"shm_size 1g", 1000 * mb, 64 * mb, 2, false, "parallel index builds allowed"},
		{"large maintenance_work_mem on 1g", 1000 * mb, 1024 * mb, 2, true, "shm_size"},
		{"shm unknown (no container)", -1, 64 * mb, 2, true, "cannot be measured"},
		{"server already serial", 10 * mb, 64 * mb, 0, false, "serial already"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := decideIndexBuild(c.shm, c.mwm, c.workers)
			if p.Serial() != c.serial {
				t.Fatalf("serial = %v, want %v (%+v)", p.Serial(), c.serial, p)
			}
			if c.serial && p.SessionOptions != SerialIndexBuildOptions {
				t.Fatalf("session options %q", p.SessionOptions)
			}
			if !strings.Contains(p.Reason, c.mention) {
				t.Fatalf("reason %q does not mention %q", p.Reason, c.mention)
			}
		})
	}
}

func TestParseDfAvailKB(t *testing.T) {
	out := "Filesystem     1024-blocks  Used Available Capacity Mounted on\nshm                  65536  1052     64484       2% /dev/shm\n"
	n, ok := parseDfAvailKB(out)
	if !ok || n != 64484 {
		t.Fatalf("got %d %v", n, ok)
	}
	for _, bad := range []string{"", "Filesystem 1024-blocks Used Available Capacity Mounted on", "header\nshm 1 2 x 3% /dev/shm"} {
		if _, ok := parseDfAvailKB(bad); ok {
			t.Fatalf("parsed %q", bad)
		}
	}
}

// The session options travel to pg_restore as PGOPTIONS — by name into docker exec, so the
// value never sits on a command line — and only when a plan set them.
func TestTool_PassesSessionOptions(t *testing.T) {
	p := PG{User: "u", Password: "secret", Container: "db"}
	cmd, err := p.tool(context.Background(), "pg_restore", true, "--list")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cmd.Args, "PGOPTIONS") || slices.ContainsFunc(cmd.Env, func(s string) bool { return strings.HasPrefix(s, "PGOPTIONS=") }) {
		t.Fatalf("PGOPTIONS passed without session options: %v", cmd.Args)
	}
	p.SessionOptions = SerialIndexBuildOptions
	cmd, err = p.tool(context.Background(), "pg_restore", true, "--list")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(cmd.Args, "PGOPTIONS")
	if i < 1 || cmd.Args[i-1] != "-e" || slices.Index(cmd.Args, "db") < i {
		t.Fatalf("docker args %v", cmd.Args)
	}
	if !slices.Contains(cmd.Env, "PGOPTIONS="+SerialIndexBuildOptions) {
		t.Fatal("PGOPTIONS missing from the environment")
	}
	for _, a := range cmd.Args {
		if strings.Contains(a, "max_parallel") || strings.Contains(a, "secret") {
			t.Fatalf("setting or password on the command line: %v", cmd.Args)
		}
	}
}
