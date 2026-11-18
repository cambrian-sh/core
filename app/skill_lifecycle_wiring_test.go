package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	skilldiscovery "github.com/cambrian-sh/core/internal/skill/discovery"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// No pool ⇒ no lifecycle (and so no "skills-lifecycle" capability).
func TestWireSkillLifecycle_NoPoolIsUnwired(t *testing.T) {
	svc, cur := wireSkillLifecycle(context.Background(), config.DefaultSkillsConfig(), nil, domain.NewInMemorySkillRegistry(), nil, nil, nil)
	if svc != nil || cur != nil {
		t.Fatal("a kernel without Postgres must not wire the lifecycle")
	}
}

func TestLifecycleConfig_MapsDays(t *testing.T) {
	c := config.DefaultSkillsConfig()
	c.Curator.Enabled = true
	lc := lifecycleConfig(c)
	if lc.Curator.StaleAfter.Hours() != 30*24 || lc.Curator.ArchiveAfter.Hours() != 90*24 || lc.Curator.Interval.Minutes() != 60 || !lc.ProposalsEnabled {
		t.Fatalf("mapped = %+v", lc)
	}
}

// Against a migrated Postgres (PG_TEST_DSN): the shipped skills get builtin
// records and stay served; the curator is built only when enabled.
func TestPg_WireSkillLifecycle_RecordsShippedSkills(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t) // migrates the database itself (ENG-2100)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reg := domain.NewInMemorySkillRegistry()
	shipped, err := skilldiscovery.LoadRegistry(filepath.Join("..", "skills"), reg)
	if err != nil || len(shipped) == 0 {
		t.Fatalf("shipped skills: %v (%d)", err, len(shipped))
	}
	cfg := config.DefaultSkillsConfig()
	cfg.Curator.Enabled = true
	svc, cur := wireSkillLifecycle(ctx, cfg, pool, reg, nil, shipped, nil)
	if svc == nil {
		t.Fatal("skill lifecycle not wired on the migrated test database")
	}
	if cur == nil {
		t.Fatal("curator enabled but not built")
	}
	skills, err := svc.Skills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	builtin := map[string]bool{}
	for _, s := range skills {
		if s.Source == domain.SkillSourceBuiltin {
			builtin[s.Name] = true
		}
	}
	for _, s := range shipped {
		if !builtin[s.Name] {
			t.Errorf("shipped skill %s has no builtin record", s.Name)
		}
		if _, ok := reg.Get(s.Name); !ok {
			t.Errorf("shipped skill %s no longer served", s.Name)
		}
	}
}
