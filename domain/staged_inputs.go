package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// Staged inputs: how an execution tool (the code sandbox) sees a person's files
// (owner decision 2026-11-10, option C;
// cambrian-knowledge/decisions/code-sandbox-files-per-run-mount.md).
//
// A tool whose operator policy declares staged_inputs (SystemTool.StagedInputs)
// takes an `inputs` argument: ledger ids, and paths the person's own file tools
// can read. For each call the kernel resolves every input UNDER THE CALLER'S
// ACCESS — a ledger id through the enforced ledger reader, a path through the
// person's file-reading tool via this executor (grant, scope, ledger record) — and
// writes exactly those files into a fresh per-run folder. The folder reaches the
// tool server out of band (MCP request _meta, never a model-visible argument); the
// server mounts it read-only for that one execution, with a writable outputs
// folder beside it. After the call the kernel records every file the tool wrote as
// an artifact ledger entry derived from the inputs, and deletes both folders.
// An input the caller cannot read refuses the whole call: nothing is staged.

// StagedInputsMetaKey* are the request _meta keys the kernel hands an execution
// tool server (tools/code_sandbox/server.py reads them). The model never sets
// _meta: the MCP handler builds it from ToolCall.Meta, which only the executor
// fills.
const (
	// StagedInputsMetaDir is the host folder holding the staged inputs (mount it read-only).
	StagedInputsMetaDir = "cambrian/inputs_dir"
	// StagedInputsMetaOutputs is the host folder the tool may write output files into.
	StagedInputsMetaOutputs = "cambrian/outputs_dir"
	// StagedInputsMetaFiles maps each requested input (as the model wrote it) to
	// its file name inside the inputs folder.
	StagedInputsMetaFiles = "cambrian/inputs"
)

// StageRequest is one call's staging request.
type StageRequest struct {
	Call ToolCallRequest
	Tool SystemTool
	// Inputs are the call's requested inputs, in order, as the model wrote them.
	Inputs []string
	// Read runs a tool call as the same caller through the executor (the path
	// reader). It goes through every gate a direct call would.
	Read func(ctx context.Context, req ToolCallRequest) ToolCallResponse
}

// StagedOutput is one file a staged tool wrote into its outputs folder.
type StagedOutput struct {
	// Name is the path relative to the outputs folder (forward slashes).
	Name        string
	ContentType string
	Body        []byte
	// Skipped says why the file was not collected (over a cap, unreadable); Body is nil then.
	Skipped string
}

// StagedCall is a staged call's folders, live until Cleanup.
type StagedCall interface {
	// Meta is the request _meta the tool server receives.
	Meta() map[string]any
	// Sources are the ledger ids the staged files came from (the derivation of
	// every entry the call produces).
	Sources() []string
	// Outputs reads the files the tool wrote.
	Outputs() []StagedOutput
	// Cleanup deletes the per-run folders. Idempotent.
	Cleanup()
}

// ToolInputStager resolves and stages a call's inputs (internal/stagedinputs).
type ToolInputStager interface {
	// Argument is the name of the inputs argument ("inputs").
	Argument() string
	// Stage resolves every input under the caller's access (ctx carries the
	// caller's read predicate) and writes them into a fresh per-run folder. Any
	// unreadable input is an error and nothing is left on disk.
	Stage(ctx context.Context, in StageRequest) (StagedCall, error)
}

// StagedInputsOf returns the inputs argument of a call (a JSON array of strings,
// or a single string), trimmed, empty entries dropped. ok=false when the argument
// is present but not one of those shapes.
func StagedInputsOf(argsJSON []byte, argument string) ([]string, bool) {
	var args map[string]json.RawMessage
	if len(argsJSON) == 0 || json.Unmarshal(argsJSON, &args) != nil {
		return nil, true
	}
	raw, present := args[argument]
	if !present || string(raw) == "null" {
		return nil, true
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		var one string
		if json.Unmarshal(raw, &one) != nil {
			return nil, false
		}
		list = []string{one}
	}
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, true
}

type ledgerRefWantedKey struct{}

// withLedgerRefWanted asks the executor to return the ledger id of a recorded
// result in ResultCID even when the result travels inline (the stager's path
// reads need it to derive the call's outputs from them).
func withLedgerRefWanted(ctx context.Context) context.Context {
	return context.WithValue(ctx, ledgerRefWantedKey{}, true)
}

func ledgerRefWanted(ctx context.Context) bool {
	v, _ := ctx.Value(ledgerRefWantedKey{}).(bool)
	return v
}

// DefaultStagedInputsArgument is the inputs argument's name when no stager is
// wired (the refusal still has to find the argument).
const DefaultStagedInputsArgument = "inputs"

// stageInputs stages a StagedInputs tool's inputs. stop=true ends the call with
// resp (a malformed argument, no stager, an unreadable input). A call with no
// inputs runs unstaged (sc == nil, stop == false).
func (e *ToolExecutor) stageInputs(ctx context.Context, req ToolCallRequest, tool SystemTool, argHash string) (sc StagedCall, resp ToolCallResponse, stop bool) {
	arg := DefaultStagedInputsArgument
	if e.Stager != nil {
		arg = e.Stager.Argument()
	}
	inputs, ok := StagedInputsOf(req.ArgsJSON, arg)
	if !ok {
		msg := fmt.Sprintf("%s must be a list of ledger ids (led_…) or file paths", arg)
		return nil, ToolCallResponse{Error: msg, ArgHash: argHash, ResultHash: hashString(msg)}, true
	}
	if len(inputs) == 0 {
		return nil, ToolCallResponse{}, false
	}
	if e.Stager == nil {
		return nil, denied("inputs cannot be staged: this kernel stages no tool inputs (execution.tools.staged_inputs.enabled is off)", argHash), true
	}
	eff, reason := e.callerReadScope(ctx, req)
	if eff == nil {
		return nil, denied("inputs refused: "+reason, argHash), true
	}
	sc, err := e.Stager.Stage(WithScope(ctx, eff), StageRequest{
		Call: req, Tool: tool, Inputs: inputs,
		Read: func(rctx context.Context, r ToolCallRequest) ToolCallResponse {
			return e.Execute(withLedgerRefWanted(rctx), r)
		},
	})
	if err != nil {
		slog.Info("staged inputs refused", "tool", req.ToolName, "agent", req.AgentID, "err", err)
		return nil, denied("inputs refused: "+err.Error(), argHash), true
	}
	return sc, ToolCallResponse{}, false
}

// recordStagedOutputs records each file a staged call wrote as an artifact
// ledger entry derived from the call's inputs (so it is never easier to read
// than they are), and appends a note naming the entries to an MCP content-array
// result so the model can cite them. Best-effort: a failed record is logged and
// named as unrecorded; the call itself already succeeded.
//
// recorded maps the sha256 of each recorded file's bytes to its entry id, so a
// binary block of the result that IS one of those files (the code sandbox returns
// a chart it also saved) becomes a reference to that entry instead of a second
// copy (referenceBinaryBlocks; T-20261112-worker-resolve-cid-inline-has-no-size-cap-a).
func (e *ToolExecutor) recordStagedOutputs(ctx context.Context, req ToolCallRequest, tool SystemTool, sc StagedCall, result []byte) (_ []byte, recorded map[string]string) {
	outs := sc.Outputs()
	if len(outs) == 0 {
		return result, nil
	}
	recorded = make(map[string]string, len(outs))
	lines := make([]string, 0, len(outs))
	for _, o := range outs {
		switch {
		case o.Skipped != "":
			lines = append(lines, fmt.Sprintf("%s: not kept (%s)", o.Name, o.Skipped))
			continue
		case e.Ledger == nil || req.System:
			lines = append(lines, fmt.Sprintf("%s: %d bytes, not recorded (the run ledger is off)", o.Name, len(o.Body)))
			continue
		}
		pathArg, _ := json.Marshal(map[string]string{"path": "outputs/" + o.Name})
		ent, err := e.Ledger.Record(ctx, LedgerRecord{
			Kind: LedgerArtifact, Provenance: LedgerProvenanceFromContext(ctx),
			ToolName: req.ToolName, ArgsJSON: pathArg, Writer: AgentPrincipal(req.AgentID),
			ClassificationHint: tool.AuthzTags(), DerivedFrom: sc.Sources(),
			ContentType: o.ContentType, Body: o.Body,
		})
		if err != nil {
			slog.Warn("staged output not recorded to the ledger", "tool", req.ToolName, "file", o.Name, "err", err)
			lines = append(lines, fmt.Sprintf("%s: %d bytes, not recorded (%v)", o.Name, len(o.Body), err))
			continue
		}
		recorded[hashBytes(o.Body)] = ent.ID
		lines = append(lines, fmt.Sprintf("%s: %d bytes, ledger %s", o.Name, len(o.Body), ent.ID))
	}
	note := "Output files (recorded by the kernel; cite the ledger id):\n" + strings.Join(lines, "\n")
	var blocks []json.RawMessage
	if json.Unmarshal(result, &blocks) != nil {
		return result, recorded // not an MCP content array: the entries are recorded, the result is left as it is
	}
	block, _ := json.Marshal(map[string]string{"type": "text", "text": note})
	blocks = append(blocks, block)
	if b, err := json.Marshal(blocks); err == nil {
		return b, recorded
	}
	return result, recorded
}

// referenceBinaryBlocks makes every binary block (image, audio, embedded blob) of
// an MCP content-array result a one-line ledger reference before the result is
// recorded or returned (domain.ReferenceBinaryBlocks). A block whose bytes are a
// file the call's staged outputs already recorded points at THAT entry; any other
// is recorded once as an artifact entry derived from the call's inputs. With the
// ledger off the block is still replaced — base64 is unreadable to a text model
// and was the whole 300k of p06 — with a note saying it was not kept. Text blocks
// are never touched.
func (e *ToolExecutor) referenceBinaryBlocks(ctx context.Context, req ToolCallRequest, tool SystemTool, result []byte, recorded map[string]string, derivedFrom []string) []byte {
	out, _ := ReferenceBinaryBlocks(result, func(b BinaryBlock) (string, string) {
		if id := recorded[hashBytes(b.Bytes)]; id != "" {
			return id, ""
		}
		if e.Ledger == nil {
			return "", "the run ledger is off"
		}
		args, _ := json.Marshal(map[string]string{"block": b.Kind})
		ent, err := e.Ledger.Record(ctx, LedgerRecord{
			Kind: LedgerArtifact, Provenance: LedgerProvenanceFromContext(ctx),
			ToolName: req.ToolName, ArgsJSON: args, Writer: AgentPrincipal(req.AgentID),
			ClassificationHint: tool.AuthzTags(), DerivedFrom: derivedFrom,
			ContentType: b.ContentType, Body: b.Bytes,
		})
		if err != nil {
			slog.Warn("binary tool-result block not recorded to the ledger", "tool", req.ToolName, "type", b.ContentType, "err", err)
			return "", "not recorded (" + err.Error() + ")"
		}
		return ent.ID, ""
	})
	return out
}
