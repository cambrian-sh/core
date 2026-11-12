package config_test

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// T-20261112-write-target-coverage-reads-title-pdf-from-a: a move, rename or copy
// is recorded by where it puts the thing (its destination), never by its source,
// so coverage can match it against a checklist item's declared target. Other
// tools keep their rules.
func TestDefaultWriteTargets_MoveRecordsItsDestination(t *testing.T) {
	wt := (*config.WriteTargetsConfig)(nil).Domain()
	cases := map[string]struct{ tool, args, want string }{
		"move_file": {"mcp:filesystem/move_file",
			`{"source":"C:\\Users\\demo\\Downloads\\scan_0041.pdf","destination":"C:\\Users\\demo\\Documents\\Receipts\\2026-10-03 Northwind Invoice.pdf"}`,
			`move_file|destination=c:\users\demo\documents\receipts\2026-10-03 northwind invoice.pdf`},
		"copy_file":             {"mcp:fs/copy_file", `{"source":"a.txt","destination":"b/a.txt"}`, "copy_file|destination=b/a.txt"},
		"rename":                {"mcp:fs/rename_file", `{"path":"a.txt","new_path":"b.txt"}`, "rename_file|new_path=b.txt"},
		"write_file":            {"mcp:filesystem/write_file", `{"path":"out/pricing.csv","content":"x"}`, "write_file|path=out/pricing.csv"},
		"mail keeps recipients": {"mcp:mail/send_email", `{"to":"a@x.com","subject":"s"}`, "send_email|to=a@x.com"},
	}
	for name, c := range cases {
		if got := wt.Key(c.tool, []byte(c.args)); got != c.want {
			t.Errorf("%s: target = %q, want %q", name, got, c.want)
		}
	}
}
