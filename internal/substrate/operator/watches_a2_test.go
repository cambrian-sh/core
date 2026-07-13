package operator_test

import (
	"context"
	"fmt"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

type fakeWatchHandler struct {
	watches map[string]domain.WatchConfig
	seq     int
}

func newFakeWatchHandler() *fakeWatchHandler {
	return &fakeWatchHandler{watches: map[string]domain.WatchConfig{}}
}
func (f *fakeWatchHandler) RegisterWatch(cfg domain.WatchConfig) (string, error) {
	f.seq++
	id := fmt.Sprintf("w%d", f.seq)
	cfg.ID = id
	f.watches[id] = cfg
	return id, nil
}
func (f *fakeWatchHandler) ListWatches() ([]domain.WatchConfig, error) {
	out := make([]domain.WatchConfig, 0, len(f.watches))
	for _, w := range f.watches {
		out = append(out, w)
	}
	return out, nil
}
func (f *fakeWatchHandler) DeleteWatch(id string) error {
	delete(f.watches, id)
	return nil
}
func (f *fakeWatchHandler) SetWatchActive(id string, active bool) error {
	w := f.watches[id]
	w.Active = active
	f.watches[id] = w
	return nil
}

func newWatchService(h domain.WatchConfigHandler) *operator.Service {
	feed := operator.NewSpool(operator.SpoolConfig{})
	svc := operator.NewService(feed)
	svc.SetCommandSources(operator.NewInMemoryAuditStore(), domain.NewInMemoryGrantsStore())
	svc.SetWatchHandler(h)
	return svc
}

// In an OSS build (no watch handler wired) all four watch RPCs are Unimplemented
// — the D14/A2.6 excisability contract.
func TestWatches_UnimplementedInOSS(t *testing.T) {
	svc := newWatchService(nil)
	if _, err := svc.ListWatches(context.Background(), &pb.ListWatchesOpRequest{}); err == nil {
		t.Error("ListWatches should be Unimplemented in OSS")
	}
	if _, err := svc.RegisterWatch(opCtx(), &pb.RegisterWatchOpRequest{CommandId: "c", Reason: "r"}); err == nil {
		t.Error("RegisterWatch should be Unimplemented in OSS")
	}
	if _, err := svc.DeleteWatch(opCtx(), &pb.DeleteWatchOpRequest{CommandId: "c", Reason: "r", WatchId: "w1"}); err == nil {
		t.Error("DeleteWatch should be Unimplemented in OSS")
	}
	if _, err := svc.SetWatchActive(opCtx(), &pb.SetWatchActiveOpRequest{CommandId: "c", Reason: "r", WatchId: "w1"}); err == nil {
		t.Error("SetWatchActive should be Unimplemented in OSS")
	}
}

// With a premium handler wired, register validates dispatch_agent, then the CRUD
// round-trips through ListWatches. A2.6.
func TestWatches_RegisterListDeleteToggle(t *testing.T) {
	h := newFakeWatchHandler()
	svc := newWatchService(h)

	// dispatch_agent without target_type is rejected.
	if _, err := svc.RegisterWatch(opCtx(), &pb.RegisterWatchOpRequest{
		CommandId: "bad", Reason: "r",
		Config: &pb.WatchConfigOp{Name: "w", Action: &pb.WatchActionOp{Type: "dispatch_agent"}},
	}); err == nil {
		t.Fatal("dispatch_agent without target_type must be InvalidArgument")
	}

	// Valid registration.
	if _, err := svc.RegisterWatch(opCtx(), &pb.RegisterWatchOpRequest{
		CommandId: "r1", Reason: "watch prices",
		Config: &pb.WatchConfigOp{
			Name: "price_watch", Condition: "price > 5000", ConditionType: "deterministic",
			Action: &pb.WatchActionOp{Type: "dispatch_agent", TargetType: "capability", Target: "trader"},
			Active: true,
		},
	}); err != nil {
		t.Fatalf("RegisterWatch: %v", err)
	}

	list, err := svc.ListWatches(context.Background(), &pb.ListWatchesOpRequest{})
	if err != nil || list.GetTotal() != 1 {
		t.Fatalf("want 1 watch, got %d err=%v", list.GetTotal(), err)
	}
	w := list.GetConfigs()[0]
	if w.GetName() != "price_watch" || w.GetAction().GetType() != "dispatch_agent" || w.GetAction().GetTarget() != "trader" || w.GetCondition() != "price > 5000" {
		t.Fatalf("watch mapping wrong: %+v", w)
	}
	id := w.GetId()

	// Toggle inactive, then active_only filter hides it.
	if _, err := svc.SetWatchActive(opCtx(), &pb.SetWatchActiveOpRequest{CommandId: "a1", Reason: "pause", WatchId: id, Active: false}); err != nil {
		t.Fatalf("SetWatchActive: %v", err)
	}
	active, _ := svc.ListWatches(context.Background(), &pb.ListWatchesOpRequest{ActiveOnly: true})
	if active.GetTotal() != 0 {
		t.Fatalf("active_only should hide the paused watch, got %d", active.GetTotal())
	}

	// Delete removes it.
	if _, err := svc.DeleteWatch(opCtx(), &pb.DeleteWatchOpRequest{CommandId: "d1", Reason: "gone", WatchId: id}); err != nil {
		t.Fatalf("DeleteWatch: %v", err)
	}
	gone, _ := svc.ListWatches(context.Background(), &pb.ListWatchesOpRequest{})
	if gone.GetTotal() != 0 {
		t.Fatalf("watch should be deleted, got %d", gone.GetTotal())
	}
}

// A replayed RegisterWatch command_id does not create a second watch (A2.6).
func TestRegisterWatch_Idempotent(t *testing.T) {
	h := newFakeWatchHandler()
	svc := newWatchService(h)
	req := &pb.RegisterWatchOpRequest{
		CommandId: "once", Reason: "r",
		Config: &pb.WatchConfigOp{Name: "w", Action: &pb.WatchActionOp{Type: "emit_event"}},
	}
	if _, err := svc.RegisterWatch(opCtx(), req); err != nil {
		t.Fatal(err)
	}
	ack, err := svc.RegisterWatch(opCtx(), req)
	if err != nil || !ack.GetDeduped() {
		t.Fatalf("replay should be deduped, got ack=%+v err=%v", ack, err)
	}
	if len(h.watches) != 1 {
		t.Fatalf("replay must not register a second watch, have %d", len(h.watches))
	}
}
