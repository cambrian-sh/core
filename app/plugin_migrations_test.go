package app

import (
	"strings"
	"testing"
	"testing/fstest"
)

// Registry.AddMigrations (T-20261113-premium-has-no-schema-migration-chain-41-create):
// a plugin's migration lane reaches the composition, two plugins of one product may
// register the SAME lane, and a conflicting or malformed lane fails registration.

type migrationsPlugin struct {
	name string
	ns   string
	fs   fstest.MapFS
}

func (p *migrationsPlugin) Manifest() PluginManifest {
	return PluginManifest{ID: p.name, DisplayName: p.name, Version: "test"}
}

func (p *migrationsPlugin) Register(r *Registry) error { return r.AddMigrations(p.ns, p.fs) }

func sqlFS(body string) fstest.MapFS {
	return fstest.MapFS{"0001_baseline.sql": &fstest.MapFile{Data: []byte(body)}}
}

func TestAddMigrations_LaneReachesTheCompositionOnceForASharedNamespace(t *testing.T) {
	lane := sqlFS("CREATE TABLE IF NOT EXISTS t (id TEXT);")
	composed, err := applyPlugins(Options{Plugins: []Plugin{
		&migrationsPlugin{name: "authz", ns: "premium", fs: lane},
		&migrationsPlugin{name: "ingress", ns: "premium", fs: sqlFS("CREATE TABLE IF NOT EXISTS t (id TEXT);")},
	}})
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if len(composed.migrations) != 1 || composed.migrations[0].Namespace != "premium" {
		t.Fatalf("want one premium lane, got %+v", composed.migrations)
	}
}

func TestAddMigrations_ConflictingLaneFailsRegistration(t *testing.T) {
	_, err := applyPlugins(Options{Plugins: []Plugin{
		&migrationsPlugin{name: "authz", ns: "premium", fs: sqlFS("SELECT 1;")},
		&migrationsPlugin{name: "ingress", ns: "premium", fs: sqlFS("SELECT 2;")},
	}})
	if err == nil || !strings.Contains(err.Error(), "already registered with different files") {
		t.Fatalf("a second, different lane under one namespace must fail: %v", err)
	}
}

func TestAddMigrations_MalformedLaneFailsRegistration(t *testing.T) {
	for name, p := range map[string]*migrationsPlugin{
		"reserved namespace": {name: "x", ns: "core", fs: sqlFS("SELECT 1;")},
		"bad file name":      {name: "x", ns: "premium", fs: fstest.MapFS{"baseline.sql": &fstest.MapFile{Data: []byte("x")}}},
		"no files":           {name: "x", ns: "premium", fs: fstest.MapFS{}},
	} {
		if _, err := applyPlugins(Options{Plugins: []Plugin{p}}); err == nil {
			t.Errorf("%s: registration succeeded", name)
		}
	}
}

func TestAddMigrations_OSSHasNoLanes(t *testing.T) {
	composed, err := applyPlugins(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(composed.migrations) != 0 {
		t.Fatalf("OSS composes no plugin lanes: %+v", composed.migrations)
	}
}
