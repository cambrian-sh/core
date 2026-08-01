package app

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/storage"
)

// CAMBRIAN_CONFIG_STORE=off is the escape hatch the kernel's OWN boot error
// recommends:
//
//	"Refusing to boot: continuing would silently ignore every setting stored there.
//	 Restore it from a backup, or set CAMBRIAN_CONFIG_STORE=off to boot from the
//	 config files alone."
//
// So the documented recovery path from a corrupt config store must not itself
// crash. It did: `OpenConfigStore` returns `(*storage.BoltConfigStore, error)`
// and yields a TYPED nil on "off". Assigned into the `config.Store` INTERFACE
// parameter, that becomes a non-nil interface holding a nil pointer — the classic
// Go trap — so `LoadConfigWithStore`'s `if store != nil` guard passed and
// `Overrides()` dereferenced `s.db` on a nil receiver.
//
// The guard was correct. The value lied to it.
func TestConfigStoreOff_TypedNilDoesNotBecomeANonNilInterface(t *testing.T) {
	var ptr *storage.BoltConfigStore // exactly what OpenConfigStore returns for "off"

	// The bug, reproduced directly: boxing a typed nil pointer into the interface.
	var boxed config.Store = ptr
	if boxed == nil {
		t.Fatal("premise broken: a typed nil pointer used to box into a non-nil " +
			"interface; if that changed, this whole test is obsolete")
	}

	// The fix: the helper must yield an interface that is genuinely nil, so every
	// downstream `store != nil` check means what it says.
	if got := configStoreOrNil(ptr); got != nil {
		t.Fatalf("configStoreOrNil(nil pointer) returned a non-nil interface (%T); "+
			"LoadConfigWithStore will call Overrides() on a nil receiver and panic", got)
	}
}

// A real store must still be passed through — a fix that nils everything would
// silently disable the config store, which is the failure the boot error exists
// to prevent in the first place.
func TestConfigStoreOff_RealStoreIsStillPassedThrough(t *testing.T) {
	dir := t.TempDir()
	real, err := storage.OpenConfigStore(dir + "/config.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = real.Close() }()

	got := configStoreOrNil(real)
	if got == nil {
		t.Fatal("a real store was nilled out; the config store would be silently ignored")
	}
	if _, err := got.Overrides(); err != nil {
		t.Fatalf("Overrides() on the passed-through store: %v", err)
	}
}

// End to end through the actual loader, which is where the panic surfaced.
func TestConfigStoreOff_LoadConfigWithStoreSurvivesADisabledStore(t *testing.T) {
	var ptr *storage.BoltConfigStore
	dir := t.TempDir()

	// Must not panic. A missing config.json is a separate, legitimate error;
	// this asserts only that we get an ERROR or a config back rather than a crash.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("LoadConfigWithStore panicked with a disabled store: %v", r)
		}
	}()
	_, _, _ = config.LoadConfigWithStore(dir+"/config.json", configStoreOrNil(ptr))
}
