package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// writeRelationsConfig writes a minimal valid config carrying the given
// top-level `relations` list (D-W5-1).
func writeRelationsConfig(t *testing.T, relationsBlock string) string {
	t.Helper()
	raw := `{
		"llm": {"endpoint":"http://localhost:11434","model":"llama3"},
		"database": {"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},
		"server": {"port":"50051"}`
	if relationsBlock != "" {
		raw += `,
		"relations": ` + relationsBlock
	}
	raw += "\n\t}"
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// The default a deployment inherits by saying nothing: no configured verbs at
// all, which NewRelationRegistry turns into the two built-in seeds. An empty
// vocabulary is a working deployment, not a broken one.
func TestRelations_AbsentSectionDeclaresNothing(t *testing.T) {
	cfg, err := config.LoadConfig(writeRelationsConfig(t, ""))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if specs := cfg.RelationSpecs(); len(specs) != 0 {
		t.Fatalf("an absent relations section declared %d verbs: %v", len(specs), specs)
	}
	reg, err := domain.NewRelationRegistry(cfg.RelationSpecs())
	if err != nil {
		t.Fatalf("the seeds alone must build: %v", err)
	}
	if _, ok := reg.Spec(domain.RelationSameAs); !ok {
		t.Fatal("the built-in seeds are missing from a registry built over an empty config")
	}
}

// The whole point of the section: a deployment's own verbs reach the registry
// without a Go change, carrying every field a RelationSpec has.
func TestRelations_ConfiguredVerbsReachTheRegistry(t *testing.T) {
	cfg, err := config.LoadConfig(writeRelationsConfig(t, `[
		{"name":"subsidiary_of","family":"relation","max_per_entity":4},
		{"name":"duplicate_of","family":"identity","symmetric":true,"closure":"identity"},
		{"name":"  fulfilled_by  ","family":" lineage "}
	]`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	specs := cfg.RelationSpecs()
	if len(specs) != 3 {
		t.Fatalf("want 3 configured verbs, got %d: %v", len(specs), specs)
	}
	// Order is preserved so a boot refusal can name the entry an operator wrote.
	if specs[0].Name != "subsidiary_of" || specs[0].Family != domain.LinkFamilyRelation ||
		specs[0].MaxPerEntity != 4 || specs[0].Symmetric || specs[0].Closure != "" {
		t.Fatalf("relations[0] did not round-trip: %+v", specs[0])
	}
	if specs[1].Name != "duplicate_of" || !specs[1].Symmetric || specs[1].Closure != domain.ClosureIdentity {
		t.Fatalf("relations[1] did not round-trip: %+v", specs[1])
	}
	// Hand-edited JSON: a trailing space must not mint a verb nothing can match.
	if specs[2].Name != "fulfilled_by" || specs[2].Family != domain.LinkFamilyLineage {
		t.Fatalf("relations[2] was not trimmed: %+v", specs[2])
	}

	reg, err := domain.NewRelationRegistry(specs)
	if err != nil {
		t.Fatalf("configured verbs must build a registry: %v", err)
	}
	spec, ok := reg.Spec("subsidiary_of")
	if !ok {
		t.Fatal("a configured verb is not declared in the registry it was folded into")
	}
	if spec.MaxPerEntity != 4 {
		t.Errorf("MaxPerEntity did not survive the fold: %+v", spec)
	}
	// A configured identity verb is what the closure walks — the read path asks
	// the registry, never a name.
	closure := reg.ClosureVerbs(domain.ClosureIdentity)
	if len(closure) != 2 {
		t.Fatalf("closure verbs = %v, want same_as plus the configured duplicate_of", closure)
	}
}

// Malformed entries refuse the BOOT rather than being dropped, filtered or
// defaulted — and they refuse in NewRelationRegistry, which is the one place
// that decides what a verb may be. config does not second-guess it.
func TestRelations_MalformedEntriesRefuseAtRegistryBuild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
		want  string
	}{
		{"no name", `[{"name":"","family":"relation"}]`, "must name its verb"},
		{"unknown family", `[{"name":"owns","family":"ownership"}]`, "unknown family"},
		{"unknown closure", `[{"name":"owns","family":"relation","closure":"transitive"}]`, "unknown closure"},
		{"negative cap", `[{"name":"owns","family":"relation","max_per_entity":-1}]`, "negative MaxPerEntity"},
		{"redeclared seed", `[{"name":"same_as","family":"identity"}]`, "built in and cannot be redeclared"},
		{"declared twice", `[{"name":"owns","family":"relation"},{"name":"owns","family":"relation"}]`, "declared twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.LoadConfig(writeRelationsConfig(t, tc.block))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			_, rerr := domain.NewRelationRegistry(cfg.RelationSpecs())
			if rerr == nil {
				t.Fatal("a malformed relations entry built a registry — the boot would have started with a vocabulary nobody agreed")
			}
			if !strings.Contains(rerr.Error(), tc.want) {
				t.Fatalf("refusal %q does not name the problem (%q)", rerr.Error(), tc.want)
			}
		})
	}
}

// Plugins and config declare into the SAME vocabulary, and a collision between
// them is a boot refusal rather than a silent winner: two owners for one verb
// is a fight the boot must referee (app.go folds config last, so the message
// names the entry an operator can edit).
func TestRelations_CollidesWithAPluginDeclaration(t *testing.T) {
	cfg, err := config.LoadConfig(writeRelationsConfig(t,
		`[{"name":"referenced_by","family":"lineage"}]`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pluginSpecs := []domain.RelationSpec{{Name: "referenced_by", Family: domain.LinkFamilyLineage}}
	if _, err := domain.NewRelationRegistry(append(pluginSpecs, cfg.RelationSpecs()...)); err == nil {
		t.Fatal("a config verb silently overwrote a plugin's declaration of the same verb")
	}
}
