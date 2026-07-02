package kernel

import (
	"io"
	"path/filepath"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/storage"
)

// contentStoreFSDir returns the filesystem directory for large blobs
// routed out of BBolt by BBoltContentStore.
func contentStoreFSDir(cfg *config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "content_store_blobs")
}

// StorageHandle is the opaque handle to raw storage returned by BootstrapStorage.
// Callers may Close() it but cannot access storage internals.
type StorageHandle struct {
	closer       io.Closer
	raw          *storage.BBoltAdapter    // only accessible within kernel package
	ContentStore domain.ContentStore      // ADR-0022: plan-trace CAS (separate file from agent registry)
	StepCache    domain.StepCache         // ADR-0026: step-level memoization (same BBolt file as agent registry)
}

// Close implements io.Closer.
func (s *StorageHandle) Close() error {
	if s.closer == nil {
		return nil
	}
	return s.closer.Close()
}

// BootstrapStorage initialises the bbolt adapter, runs auto-discovery, and
// returns both the raw storage handle and the domain-facing decorator.
// This is the ONLY place in the application that constructs storage.BBoltAdapter.
//
// ADR-0022: also constructs BBoltContentStore using a separate database file
// so it doesn't contend with the agent-registry bbolt file lock.
func BootstrapStorage(cfg *config.Config) (*StorageHandle, *AgentRepoDecorator, error) {
	dbPath := filepath.Join(cfg.Storage.DataDir, cfg.Storage.DBName)
	store, err := storage.NewBBoltAdapter(dbPath, cfg.Metabolism.AgentsDir, domain.IsSystemAgent)
	if err != nil {
		return nil, nil, err
	}

	// ADR-0022 Phase 1: content store uses a separate bbolt file — same directory,
	// different file to avoid bbolt's single-writer file-lock conflict.
	csPath := filepath.Join(cfg.Storage.DataDir, "content_store.db")
	cs, err := storage.NewBBoltContentStore(csPath, contentStoreFSDir(cfg))
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}

	sc, err := store.NewStepCache()
	if err != nil {
		_ = store.Close()
		_ = cs.Close()
		return nil, nil, err
	}

	decorator := NewAgentRepoDecorator(store)
	return &StorageHandle{
		closer:       multiCloser{store, cs},
		raw:          store,
		ContentStore: cs,
		StepCache:    sc,
	}, decorator, nil
}

// multiCloser closes multiple io.Closers in order, collecting errors.
type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var last error
	for _, c := range m {
		if err := c.Close(); err != nil {
			last = err
		}
	}
	return last
}

// WireInterviewEnqueuer connects the interview worker to storage callbacks.
// When BBoltAdapter discovers a new A2A agent, it enqueues it for interview.
func (h *StorageHandle) WireInterviewEnqueuer(enqueuer func(agent domain.AgentDefinition)) {
	if h.raw == nil {
		return
	}
	h.raw.Enqueuer = funcEnqueuer(enqueuer)
}

// funcEnqueuer adapts a plain function to the storage.Enqueuer interface
// without exposing the storage package to callers.
type funcEnqueuer func(domain.AgentDefinition)

func (f funcEnqueuer) Enqueue(agent domain.AgentDefinition) { f(agent) }
