package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The lane's two published tools (ADR-0127 D3): `poll_step` and `report_step`,
// the ONLY verbs a worker has. They ride the ADR-0126 Published Tool Surface
// like any other published tool but are callable ONLY by machine:* principals
// — enforced here, in the handler, from the authenticated context (INV-5:
// identity is bound by the transport middleware before any handler runs;
// arguments are never a trust input for it).
//
// They are transport, not contribution: nothing a worker offers through them
// ever appears on the outward surface (parity-ledger exclusion 3) — the
// manifest flows INWARD into the fleet, and only the beneficiary's own task
// menus resolve it.

// workerPrincipal extracts the authenticated machine and its owner from ctx,
// refusing everyone else. One helper so poll and report cannot drift on what
// "a worker" means.
func workerPrincipal(ctx context.Context) (machine string, owner PrincipalRef, err error) {
	p := PrincipalFromContext(ctx)
	if p.Kind != PrincipalMachine || p.ID == "" {
		return "", PrincipalRef{}, errors.New("this tool is callable only by worker machines (machine:* principals; issue a credential with `cambrian mcp token create <machine> --owner <owner-principal>`)")
	}
	owner = WorkerOwnerFromContext(ctx)
	if owner.IsZero() {
		// A machine principal with no owner on the context is a wiring defect,
		// not a caller error — the middleware only mints machine principals
		// FROM an owner binding — but an ownerless worker must still refuse:
		// a fleet entry nobody can be the beneficiary of is a dangling grant.
		return "", PrincipalRef{}, errors.New("worker credential carries no owner principal; re-issue it with --owner")
	}
	return p.ID, owner, nil
}

// pollStepArgs is the worker's side of the declarative registration: the
// manifest it offers (re-stated on every poll) and how long it is willing to
// hold. All of it is untrusted input; names are validated, the wait is
// clamped, and the OWNER never comes from here.
type pollStepArgs struct {
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ReadOnly    bool   `json:"read_only"`
	} `json:"tools"`
	Consent string `json:"consent"`
	WaitMs  int    `json:"wait_ms"`
	// DefaultMachine claims ladder-rung-3 preference for this machine (ADR-0127
	// D6, CL-2). Additive: an older broker never sends it. The claim is scoped
	// to the owner's own fleet by construction — it can only influence which of
	// the owner's OWN machines serves the owner's own bare-capability steps.
	DefaultMachine bool `json:"default_machine"`
}

// WorkerPollTool serves poll_step.
type WorkerPollTool struct{ Hub *WorkerHub }

// Invoke long-polls for the authenticated worker.
func (t WorkerPollTool) Invoke(ctx context.Context, args json.RawMessage) (PublishedToolResult, error) {
	machine, owner, err := workerPrincipal(ctx)
	if err != nil {
		return PublishedToolResult{}, err
	}
	var a pollStepArgs
	if len(args) > 0 {
		if uerr := json.Unmarshal(args, &a); uerr != nil {
			return PublishedToolResult{}, fmt.Errorf("poll_step: bad arguments: %w", uerr)
		}
	}
	tools := make([]SystemTool, 0, len(a.Tools))
	for _, mt := range a.Tools {
		effects := []ToolEffect{EffectRead}
		if !mt.ReadOnly {
			// Conservative: a tool that does not declare itself read-only is
			// assumed to mutate. Attachment stamps egress on top regardless.
			effects = append(effects, EffectWrite)
		}
		tools = append(tools, SystemTool{Name: mt.Name, Description: mt.Description, Effects: effects})
	}
	step, got, err := t.Hub.PollOffer(ctx, WorkerRegistration{
		Machine: machine,
		Owner:   owner,
		Tools:   tools,
		Consent: WorkerConsent(a.Consent),
		Default: a.DefaultMachine,
	}, time.Duration(a.WaitMs)*time.Millisecond)
	if err != nil {
		return PublishedToolResult{}, err
	}
	if !got {
		return PublishedToolResult{
			Structured: map[string]any{"step": nil},
			Text:       "no step; poll again (holding the poll open is your liveness signal)",
		}, nil
	}
	wireStep := map[string]any{
		"id":        step.ID,
		"tool":      step.Tool,
		"args_json": string(step.ArgsJSON),
	}
	if step.Consent != "" {
		// ADDITIVE (CL-2): "consent":"on-machine" — obtain local consent before
		// executing; report a refusal via report_step's consent field. A worker
		// that ignores unknown step fields is untouched.
		wireStep["consent"] = step.Consent
	}
	return PublishedToolResult{
		Structured: map[string]any{"step": wireStep},
		Text:       "step " + step.ID + ": run " + step.Tool + " and answer with report_step",
	}, nil
}

// reportStepArgs is one step's outcome. ResultJSON is the worker's raw output
// — UNTRUSTED, and fenced (D8) by the relay before any agent context sees it;
// nothing here is interpreted kernel-side.
type reportStepArgs struct {
	StepID     string `json:"step_id"`
	ResultJSON string `json:"result_json"`
	Error      string `json:"error"`
	// Consent is the CL-2 on-machine consent outcome. Only WireConsentDenied
	// ("denied") is meaningful — it turns the report into a recorded refusal,
	// not a worker error. Anything else (including absent — every CL-1 worker)
	// is ignored.
	Consent string `json:"consent"`
}

// WorkerReportTool serves report_step.
type WorkerReportTool struct{ Hub *WorkerHub }

// Invoke completes one step for the authenticated worker. Idempotent on the
// step id: a retry after a lost response answers ok without double-applying.
func (t WorkerReportTool) Invoke(ctx context.Context, args json.RawMessage) (PublishedToolResult, error) {
	machine, _, err := workerPrincipal(ctx)
	if err != nil {
		return PublishedToolResult{}, err
	}
	var a reportStepArgs
	if uerr := json.Unmarshal(args, &a); uerr != nil {
		return PublishedToolResult{}, fmt.Errorf("report_step: bad arguments: %w", uerr)
	}
	if a.StepID == "" {
		return PublishedToolResult{}, errors.New("report_step: step_id is required")
	}
	if rerr := t.Hub.ReportOutcome(machine, a.StepID, []byte(a.ResultJSON), a.Error, a.Consent == WireConsentDenied); rerr != nil {
		return PublishedToolResult{}, rerr
	}
	return PublishedToolResult{
		Structured: map[string]any{"ok": true, "step_id": a.StepID},
		Text:       "step " + a.StepID + " reported",
	}, nil
}

// ContributionLaneTools renders the lane's transport onto the Published Tool
// Surface. Composed by the root beside the core tools; deliberately NOT part
// of mcpserve.CoreTools, whose golden freezes the four read-only memory tools.
func ContributionLaneTools(hub *WorkerHub) PublishedToolSurface {
	return PublishedToolSurface{
		{
			Owner: "contribution-lane",
			Tool: PublishedTool{
				Name:        "poll_step",
				Title:       "Poll for the next worker step",
				Description: "Worker machines only. Long-poll for the next step targeted at this machine, re-stating the manifest of locally served tools. Holding the poll open is the liveness signal; answer steps with report_step.",
				InputSchema: []byte(`{"type":"object","properties":{"tools":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"description":{"type":"string"},"read_only":{"type":"boolean"}},"required":["name"]}},"consent":{"type":"string","enum":["auto","any-surface","on-machine-only"]},"wait_ms":{"type":"integer"},"default_machine":{"type":"boolean"}}}`),
				// Honest effects: a poll registers manifest/liveness state and
				// receives work — it reads and writes kernel state, and the D8
				// listing filter narrows on exactly this.
				Effects:     []ToolEffect{EffectRead, EffectWrite},
				MachineOnly: true,
			},
			Handler: WorkerPollTool{Hub: hub},
		},
		{
			Owner: "contribution-lane",
			Tool: PublishedTool{
				Name:        "report_step",
				Title:       "Report a worker step's result",
				Description: "Worker machines only. Return one step's result (or error) by step_id. Idempotent: retrying a report after a lost response is safe.",
				InputSchema: []byte(`{"type":"object","properties":{"step_id":{"type":"string"},"result_json":{"type":"string"},"error":{"type":"string"},"consent":{"type":"string","enum":["denied"]}},"required":["step_id"]}`),
				Effects:     []ToolEffect{EffectWrite},
				MachineOnly: true,
			},
			Handler: WorkerReportTool{Hub: hub},
		},
	}
}
