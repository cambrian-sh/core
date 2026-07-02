package config

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotEnv reads a .env file and populates the process environment so that
// secrets (e.g. OPENCODE_API_KEY) and CAMBRIAN_* overrides can be supplied
// from a local, gitignored file instead of the shell.
//
// Precedence is deliberate: a variable already present in the real OS
// environment is NEVER overwritten. This lets CI / production inject secrets
// directly while developers rely on .env, with the same code path.
//
// A missing file is not an error — the function returns nil so callers can
// invoke it unconditionally at startup.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip blanks and comments.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Tolerate a leading "export " for shell-sourcing compatibility.
		line = strings.TrimPrefix(line, "export ")

		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue // malformed line without '='; skip rather than fail
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		val = strings.TrimSpace(val)
		// Strip a single layer of matching surrounding quotes.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		// Real environment wins over the file.
		if _, present := os.LookupEnv(key); present {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	return scanner.Err()
}
