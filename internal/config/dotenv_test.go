package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# comment line
export OPENCODE_API_KEY=sk-from-file
QUOTED="quoted value"
SINGLE='single value'
PRESET=should-not-override

malformed-line-without-equals
EMPTY=
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// PRESET already in the real environment must win over the file.
	t.Setenv("PRESET", "from-os")

	if err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}

	cases := map[string]string{
		"OPENCODE_API_KEY": "sk-from-file",   // export prefix stripped
		"QUOTED":           "quoted value",   // double quotes stripped
		"SINGLE":           "single value",   // single quotes stripped
		"PRESET":           "from-os",        // OS env wins
		"EMPTY":            "",               // empty value allowed
	}
	for k, want := range cases {
		// Clean up vars this test introduced (t.Setenv already handles PRESET).
		if k != "PRESET" {
			defer os.Unsetenv(k)
		}
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestLoadDotEnv_MissingFileIsNoError(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "does-not-exist.env")); err != nil {
		t.Fatalf("missing file should be a no-op, got: %v", err)
	}
}
