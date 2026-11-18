package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// The ToolExecutor side of approvals that outlive their kernel run
// (approval_intent.go; T-20261118-a-sync-approval-orphaned-by-a-kernel-restart).
//
// At the approval gate a call stores its intent: the validated arguments and
// the kernel-held request facts. When its approval is approved after the kernel
// that raised it restarted, the hub hands the intent to this recoverer, which
// runs the call through Execute again — every check (the grant, the resource
// policy, the data scope, the effect classes, the pre_tool hooks) runs anew
// against the rules in force NOW, for the original agent and person. Only the
// approval gate is satisfied by the approval the person gave, and only for the
// very call they approved (same tool, same arguments).

// preApprovedKey carries the approval a recovered call runs under. Unexported:
// only toolRecoverer sets it, after re-reading the approved item from the hub.
type preApprovedKey struct{}

type preApproval struct {
	id       string
	tool     string
	argHash  string
	approver string
}

func preApprovedFrom(ctx context.Context) (preApproval, bool) {
	pa, ok := ctx.Value(preApprovedKey{}).(preApproval)
	return pa, ok
}

// toolIntent is the intent stored with a tool call's approval. nil — so an
// approve after a restart is refused — for a call whose authority does not
// survive a restart: one made on a subtask's child lease (its effect ceiling
// and parent run are lease state, ADR-0150 D2) and a contributed local: step
// (its machine, consent and parking are live fleet state, ADR-0127).
func toolIntent(ctx context.Context, req ToolCallRequest, args []byte, subtaskLease, contributed bool) *ApprovalIntent {
	if req.System || subtaskLease || contributed {
		return nil
	}
	sid, _ := SessionIDFromContext(ctx)
	ben := TaskBeneficiaryFromContext(ctx)
	surf := SurfaceFromContext(ctx)
	in := &ApprovalIntent{
		Executor:          ApprovalIntentTool,
		AgentID:           req.AgentID,
		Tool:              req.ToolName,
		Args:              append([]byte(nil), args...),
		OnBehalfOf:        RunPrincipalFromContext(ctx),
		ConversationOwner: ConversationOwnerFromContext(ctx),
		BeneficiaryID:     ben.ID,
		BeneficiaryKind:   string(ben.Kind),
		ConversationID:    ConversationIDFromContext(ctx),
		SessionID:         string(sid),
		SurfaceID:         surf.ID,
		SurfaceKind:       surf.Kind,
		TaskID:            req.TaskID,
		Lease:             string(req.SessionTokenID),
	}
	if sc, ok := StepCallFrom(ctx); ok {
		in.HasStep, in.RunID, in.StepIndex = true, string(sc.RunID), sc.StepIndex
	}
	return in
}

// approvalReader is the hub's read of one item (AddressedHub.Get).
type approvalReader interface {
	Get(ctx context.Context, id string) (ApprovalItem, error)
}

// ApprovalRecoverer returns the recoverer of tool-call intents
// (ApprovalIntentTool), for AddressedHub.RecoverWith. directory answers whether
// the person a call was for is still a known, active principal; nil (or a nil
// directory) refuses every call made for a person — the same fail-closed rule a
// scheduled task's firing follows (app.ownedTurnRunner).
func (e *ToolExecutor) ApprovalRecoverer(directory func() PrincipalDirectory) ApprovalRecoverer {
	return &toolRecoverer{e: e, directory: directory}
}

type toolRecoverer struct {
	e         *ToolExecutor
	directory func() PrincipalDirectory
}

func (r *toolRecoverer) request(it ApprovalItem) (ToolCallRequest, error) {
	in := it.Intent
	if in == nil || in.Executor != ApprovalIntentTool || in.Tool == "" {
		return ToolCallRequest{}, errors.New("the approval does not name a tool call")
	}
	return ToolCallRequest{AgentID: in.AgentID, ToolName: in.Tool, ArgsJSON: append([]byte(nil), in.Args...), TaskID: in.TaskID}, nil
}

// CheckRecovery: the person is still known and active, the tool is still
// registered, and the call passes the grant, resource, scope and effect checks
// in force now. Execute runs all of them again; this answers before the
// approve is recorded, so a call that would be refused is withdrawn instead.
func (r *toolRecoverer) CheckRecovery(ctx context.Context, it ApprovalItem) error {
	req, err := r.request(it)
	if err != nil {
		return err
	}
	if err := r.personActive(ctx, it.Intent.Person()); err != nil {
		return err
	}
	tool, ok := r.e.Registry.Get(req.ToolName)
	if !ok {
		return fmt.Errorf("the tool %s is no longer available", req.ToolName)
	}
	if _, why, ok := r.e.authorize(it.Intent.Context(ctx), &req, tool); !ok {
		return fmt.Errorf("the access rules in force now refuse it: %s", why)
	}
	return nil
}

func (r *toolRecoverer) personActive(ctx context.Context, person string) error {
	if person == "" {
		return nil // a call made for nobody: the agent's own grants decide
	}
	var dir PrincipalDirectory
	if r.directory != nil {
		dir = r.directory()
	}
	if dir == nil {
		return fmt.Errorf("nothing can confirm that %s is still an active person", person)
	}
	known, err := dir.KnownPrincipal(ctx, person)
	if err != nil {
		return fmt.Errorf("could not confirm that %s is still an active person: %v", person, err)
	}
	if !known {
		return fmt.Errorf("%s is no longer a known, active person", person)
	}
	return nil
}

// Recover runs the approved call once through Execute.
func (r *toolRecoverer) Recover(ctx context.Context, it ApprovalItem) (string, error) {
	req, err := r.request(it)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRecoveryRefused, err)
	}
	// The approval gate is satisfied only by the approval as the hub holds it:
	// approved, and for exactly this call.
	reader, ok := r.e.approval.(approvalReader)
	if !ok {
		return "", fmt.Errorf("%w: the approval cannot be verified", ErrRecoveryRefused)
	}
	stored, err := reader.Get(ctx, it.ID)
	if err != nil || stored.State != ApprovalApproved || stored.Intent == nil ||
		stored.Intent.Tool != req.ToolName || !bytes.Equal(stored.Intent.Args, req.ArgsJSON) {
		return "", fmt.Errorf("%w: the approval does not match the call", ErrRecoveryRefused)
	}
	if err := r.personActive(ctx, it.Intent.Person()); err != nil {
		return "", fmt.Errorf("%w: %v", ErrRecoveryRefused, err)
	}
	rctx := it.Intent.Context(ctx)
	rctx = context.WithValue(rctx, preApprovedKey{}, preApproval{
		id: it.ID, tool: req.ToolName, argHash: hashBytes(req.ArgsJSON), approver: stored.ResolvedBy,
	})
	resp := r.e.Execute(rctx, req)
	switch {
	case resp.Denied:
		return "", fmt.Errorf("%w: %s", ErrRecoveryRefused, resp.DenyReason)
	case resp.Error != "":
		return "", fmt.Errorf("%s ran and failed: %s", req.ToolName, resp.Error)
	}
	out := string(resp.ResultJSON)
	if out == "" && resp.ResultCID != "" {
		out = "result stored as " + resp.ResultCID
	}
	return RecoverySummary(req.ToolName + " ran: " + out), nil
}
