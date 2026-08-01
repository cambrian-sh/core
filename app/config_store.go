package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cambrian-sh/core/internal/config"
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

// configStoreOrNil converts the concrete store pointer into a config.Store
// interface that is genuinely nil when there is no store.
//
// Without this, `CAMBRIAN_CONFIG_STORE=off` PANICKED — and it is the escape hatch
// the kernel's own boot error recommends, so the documented recovery from a
// corrupt store was itself a crash.
//
// The mechanism is the classic Go trap. `OpenConfigStore` returns
// `(*storage.BoltConfigStore, error)` and yields a typed nil for "off". Assigning
// that into an interface parameter produces an interface that is NOT nil — it
// holds a non-nil type descriptor and a nil value — so `LoadConfigWithStore`'s
// `if store != nil` guard passed and `Overrides()` dereferenced `s.db` on a nil
// receiver.
//
// The guard was never wrong; the value lied to it. Converting at the boundary is
// the fix, rather than adding nil-receiver guards to every store method: those
// would make the same mistake survivable at each new call site instead of
// impossible at the one place the conversion happens.
func configStoreOrNil(s *storage.BoltConfigStore) config.Store {
	if s == nil {
		return nil
	}
	return s
}
