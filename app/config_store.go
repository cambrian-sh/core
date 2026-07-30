package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cambrian-sh/core/internal/storage"
)

// ConfigStoreEnv names the environment variable that relocates the ADR-0101
// config store. Set it to a path to move the store; set it to "off" to disable
// the store layer entirely, which reproduces the pre-ADR-0101 config pipeline
// exactly.
//
// "off" exists for benchmark arms: the harness's authority depends on driving the
// kernel the way a user's process runs it, and an arm that must not inherit an
// operator's durable edits needs a way to say so that does not involve deleting
// the operator's file.
const ConfigStoreEnv = "CAMBRIAN_CONFIG_STORE"

// OpenConfigStore opens the config store for a kernel rooted at baseDir, or
// returns (nil, nil) when the store is disabled.
//
// It lives in the config BUNDLE (`<baseDir>/configs/config.db`) rather than the
// data directory on purpose. The data directory is the thing a corpus reset
// truncates and a container mount replaces; putting durable operator settings
// and every stored credential there would mean a routine reset silently
// un-configures the deployment.
//
// A store that exists but cannot be opened FAILS THE BOOT rather than degrading
// to "no store". Degrading would boot a kernel whose config differs from what the
// operator durably set, with nothing to indicate it — the exact silently-wrong
// outcome ADR-0101 exists to prevent. The error names the file so the fix is
// obvious.
func OpenConfigStore(baseDir string) (*storage.BoltConfigStore, error) {
	path := os.Getenv(ConfigStoreEnv)
	if path == "off" {
		return nil, nil
	}
	if path == "" {
		path = filepath.Join(baseDir, "configs", "config.db")
	}
	store, err := storage.OpenConfigStore(path)
	if err != nil {
		return nil, fmt.Errorf(
			"config store at %s could not be opened: %w\n"+
				"Refusing to boot: continuing would silently ignore every setting stored there.\n"+
				"Restore it from a backup, or set %s=off to boot from the config files alone.",
			path, err, ConfigStoreEnv)
	}
	return store, nil
}
