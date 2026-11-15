package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// memRecords is an in-memory domain.SealedRecordStore.
type memRecords struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemRecords() *memRecords { return &memRecords{m: map[string][]byte{}} }

func (r *memRecords) PutSealed(k string, v []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[k] = append([]byte(nil), v...)
	return nil
}
func (r *memRecords) GetSealed(k string) ([]byte, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.m[k]
	return v, ok, nil
}
func (r *memRecords) DeleteSealed(k string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.m[k]
	delete(r.m, k)
	return ok, nil
}
func (r *memRecords) SealedKeys() ([]string, error) { return nil, nil }

// ENG-743 (T-20261106-per-user-mcp-servers-show-no-tools-until-the): a per_user
// server's tools, once discovered, survive a restart: the next kernel publishes
// them at once — under the CURRENT policy — so before anyone has connected an
// agent finds the tool and a call answers "connect your account" (never another
// person's credential). Removing the server forgets the list.
func TestPerUser_ToolListSurvivesARestartAndPublishesBeforeAnyoneConnects(t *testing.T) {
	srv := newWhoamiServer(t, "Authorization")
	store := newMemRecords()
	cfg := perUserHTTP("mail", srv.url, "bearer", "")

	// Kernel 1: alice connects, discovery publishes and remembers the list.
	creds := newFakeUserCreds()
	creds.set("mail", "alice", "tok-alice-1111")
	first := NewConnector(CredentialSources{User: creds}, WithToolLists(store))
	ctx1, cancel1 := context.WithCancel(context.Background())
	sink1 := newRecSink()
	first.AddServer(ctx1, cfg, sink1, time.Hour)
	waitFor(t, "first discovery", func() bool { return sink1.count("mail") == 1 })
	cancel1()
	first.Close()
	if _, ok, _ := store.GetSealed("mail"); !ok {
		t.Fatal("the discovered tool list was not persisted")
	}

	// Kernel 2 (restart): nobody is connected; the policy was edited meanwhile.
	cfg.DefaultPolicy = &ToolPolicy{Effects: []string{"read", "egress"}}
	second := NewConnector(CredentialSources{User: newFakeUserCreds()}, WithToolLists(store))
	defer second.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	sink2 := newRecSink()
	second.AddServer(ctx2, cfg, sink2, time.Hour)
	waitFor(t, "known tools published before anyone connects", func() bool { return sink2.count("mail") == 1 })
	sink2.mu.Lock()
	tool := sink2.tools["mail"][0]
	sink2.mu.Unlock()
	if tool.Name != "mcp:mail/whoami" || len(tool.Effects) != 2 {
		t.Fatalf("published tool %+v: want mcp:mail/whoami under the current policy (read+egress)", tool)
	}
	waitFor(t, "awaiting_user with the known tool count", func() bool {
		h, _ := second.Health("mail")
		return h.State == HealthAwaitingUser && h.ToolCount == 1
	})
	_, err := callAs(context.Background(), &Handler{Sessions: second}, "bob", "mcp:mail/whoami")
	var nc *NotConnectedError
	if !errors.As(err, &nc) {
		t.Fatalf("a call before bob connected must say connect your account, got %v", err)
	}

	second.RemoveServer(context.Background(), "mail", sink2)
	if _, ok, _ := store.GetSealed("mail"); ok {
		t.Fatal("removing the server must forget its tool list")
	}
}

var _ domain.SealedRecordStore = (*memRecords)(nil)
