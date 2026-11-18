package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/storage"
)

// ENG-743 through the composition: the connector the kernel builds over the
// ADR-0101 store (mcpConnectorOptions) remembers a per_user server's tools in
// the store, and a kernel reopened on that store puts them on the tool registry
// before anyone has connected.
func TestMCPToolLists_PersistedInTheConfigStoreAcrossARestart(t *testing.T) {
	srv := newAuthServer(t)
	path := filepath.Join(t.TempDir(), "config.db")
	cfg := mcp.ServerConfig{ID: "gh", Transport: "http", Endpoint: srv.url, AllowPrivateNetwork: true,
		DefaultPolicy:   &mcp.ToolPolicy{Effects: []string{"read"}},
		CredentialScope: mcp.ScopePerUser, UserInject: "bearer"}

	boot := func(accounts *fakeAccounts) (*mcp.Connector, *domain.InMemoryToolRegistry, func()) {
		store, err := storage.OpenConfigStore(path)
		if err != nil {
			t.Fatal(err)
		}
		opts := mcpConnectorOptions(&config.Config{}, store)
		if len(opts) != 2 { // the timeouts + the tool lists
			t.Fatalf("an open store must persist tool lists (%d options)", len(opts))
		}
		conn := mcp.NewConnector(mcp.CredentialSources{Secrets: store, User: accounts}, opts...)
		reg := domain.NewInMemoryToolRegistry()
		sink := newMCPToolSink(reg, &domain.ToolIndexer{Store: &sinkFakeStore{}, Embedder: sinkFakeEmb{}})
		ctx, cancel := context.WithCancel(context.Background())
		conn.AddServer(ctx, cfg, sink, time.Hour)
		return conn, reg, func() { cancel(); conn.Close(); _ = store.Close() }
	}

	_, reg, stop := boot(&fakeAccounts{tokens: map[string]string{"gh/alice": "tok-alice-1"}})
	waitUntil(t, "first discovery", func() bool { _, ok := reg.Get("mcp:gh/lookup"); return ok })
	stop()

	_, reg2, stop2 := boot(&fakeAccounts{tokens: map[string]string{}})
	defer stop2()
	waitUntil(t, "tools known after restart with nobody connected", func() bool { _, ok := reg2.Get("mcp:gh/lookup"); return ok })
}
