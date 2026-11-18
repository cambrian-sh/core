package network

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func thirdPartySubtaskServer(parentAgent string) (*Server, *subtaskGateway, *subtaskDispatcher) {
	gw := &subtaskGateway{bindings: map[domain.LeaseID]domain.LeaseBinding{
		"parent": {SessionID: "ses-1", RunID: "run-1", AgentID: parentAgent},
	}}
	sub := domain.NewSubtasks(0, nil)
	sub.RegisterParent("parent", parentAgent)
	disp := &subtaskDispatcher{resp: &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: []byte("ok")}}}
	s := &Server{LLMGateway: gw, Dispatcher: disp, Subtasks: sub, Compositions: domain.NewComposedLeases(),
		ComposedRuntimeID: "composed_worker", ComposedThirdPartyRuntimeID: "composed_worker_third_party",
		ToolExecutor: &domain.ToolExecutor{Overlay: domain.NewRunGrantOverlay()}}
	return s, gw, disp
}

// ENG-2083: a subtask of a lease running on the third-party composed runtime
// runs on it too — never on the shared system-tier runtime.
func TestRunSubtask_ThirdPartyParentFansOutOnTheThirdPartyRuntime(t *testing.T) {
	for name, parentAgent := range map[string]string{
		"parent-bound-to-third-party-runtime": "composed_worker_third_party",
		"parent-composition-on-it":            "someone",
	} {
		t.Run(name, func(t *testing.T) {
			s, gw, disp := thirdPartySubtaskServer(parentAgent)
			s.Compositions.Bind("parent", domain.WorkerComposition{Runtime: "composed_worker_third_party", Professions: []string{"outsider"}})
			if _, err := s.RunSubtask(context.Background(), domain.SubtaskDispatch{ParentAgentID: parentAgent,
				Binding: domain.SubtaskBinding{SubtaskID: "s1", ParentLease: "parent", RootLease: "parent", Depth: 1},
				Task:    "x"}); err != nil {
				t.Fatal(err)
			}
			if disp.agent != "composed_worker_third_party" || gw.bindings["child-1"].AgentID != "composed_worker_third_party" {
				t.Fatalf("subtask ran on %q (child bound to %q), want the third-party runtime", disp.agent, gw.bindings["child-1"].AgentID)
			}
		})
	}
	// A shared-runtime parent still fans out on the shared runtime.
	s, _, disp := thirdPartySubtaskServer("composed_worker")
	if _, err := s.RunSubtask(context.Background(), domain.SubtaskDispatch{ParentAgentID: "composed_worker",
		Binding: domain.SubtaskBinding{SubtaskID: "s2", ParentLease: "parent", RootLease: "parent", Depth: 1}, Task: "x"}); err != nil {
		t.Fatal(err)
	}
	if disp.agent != "composed_worker" {
		t.Fatalf("shared parent's subtask ran on %q", disp.agent)
	}
}

// Both composed runtimes hold nothing without a composed lease (fail closed).
func TestComposedFor_BothRuntimesAreRuntimeOnlyWithoutALease(t *testing.T) {
	s := &Server{Compositions: domain.NewComposedLeases(), ComposedRuntimeID: "composed_worker",
		ComposedThirdPartyRuntimeID: "composed_worker_third_party"}
	for _, id := range []string{"composed_worker", "composed_worker_third_party"} {
		if _, _, runtimeOnly := s.composedFor(context.Background(), id); !runtimeOnly {
			t.Errorf("%s without a composed lease must get nothing", id)
		}
	}
	if _, _, runtimeOnly := s.composedFor(context.Background(), "researcher"); runtimeOnly {
		t.Error("a profile agent is not a composed runtime")
	}
}
