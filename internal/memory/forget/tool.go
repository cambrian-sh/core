package forget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ToolName is the agent-facing forget tool (ADR-0137 D9).
const ToolName = "forget_memory"

// Tool is forget_memory: the agent-facing tool over the same forget service the
// agent-plane PreviewForget/Forget RPCs use (ADR-0137 D9, ENG-411 stage C). It is a
// domain.KernelTool, served in process through ExecuteTool under the caller's own read
// predicate (ToolExecutor.runKernelTool puts it on ctx), so an agent forgets with
// exactly the authority of the RPC: writer-only within its read scope.
//
// Always two calls: a call WITHOUT preview_token only previews and returns a token;
// only a second call carrying that token erases, and it erases exactly the previewed
// records. The preview names either ids, or a free-text `query` (ENG-1948) that the
// kernel resolves with the caller's own memory search (Service.PreviewQuery): only hits
// that clear the relevance floor count (ENG-1953), each widens to its whole document,
// only what the caller wrote is listed, each with a clipped snippet of the caller's own
// text, at most execution.erasure.preview_max_candidates of them (`truncated` when
// cut). The kernel adds no LLM call to decide what to delete (D9).
//
// Offered (KernelToolOffer) only when erasure is enabled AND agent erasure is on
// (execution.erasure.enabled, agent_erase_enabled — both default off), so default agent
// menus are unchanged; a call by name while not offered is refused by the chokepoint.
type Tool struct {
	svc        *Service
	agentErase bool
}

// NewTool builds forget_memory over the service. agentErase is
// execution.erasure.agent_erase_enabled.
func NewTool(svc *Service, agentErase bool) *Tool { return &Tool{svc: svc, agentErase: agentErase} }

var (
	_ domain.KernelTool      = (*Tool)(nil)
	_ domain.KernelToolOffer = (*Tool)(nil)
)

// OfferedTo implements domain.KernelToolOffer.
func (t *Tool) OfferedTo(domain.LeaseID) bool { return t.svc.Enabled() && t.agentErase }

const toolSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "what to forget, in words (e.g. what the person said about their salary): your own memory is searched and each document you wrote that closely matches is previewed as a whole document; use instead of ids"},
    "document_ids": {"type": "array", "items": {"type": "string"}, "description": "documents to forget (doc ids from memory results)"},
    "chunk_ids": {"type": "array", "items": {"type": "string"}, "description": "memory ids returned earlier"},
    "thread_ids": {"type": "array", "items": {"type": "string"}, "description": "ingest threads (the session id an ingest was made under)"},
    "preview_token": {"type": "string", "description": "the token a previous forget_memory call returned; set it (and nothing else) to erase exactly what was previewed"},
    "reason_code": {"type": "string", "enum": ["user_request", "correction", "compliance"], "description": "why (default user_request)"}
  },
  "additionalProperties": false
}`

// Spec implements domain.KernelTool.
func (t *Tool) Spec() domain.SystemTool {
	return domain.SystemTool{
		Name: ToolName,
		Description: "Permanently erase memory you wrote, and lines of the memory notes of the person you are " +
			"acting for, in two calls. First call with a query describing " +
			"what to forget, or with the ids to forget (document_ids / chunk_ids / thread_ids): nothing is " +
			"erased; you get the records that would be erased, with a short snippet for each when you used " +
			"a query, and a preview_token. A query lists only documents that closely match it, at most a fixed " +
			"number of them; if the result says truncated, more matched than were listed — narrow the query " +
			"(or name ids) to reach the rest. A document a query misses can still be named by id. " +
			"Erasure is by WHOLE DOCUMENT: if any passage of a document matches, " +
			"the entire document (every passage of it) is listed and will be erased. The person's memory " +
			"notes (the notes shown in your instructions) are matched by their words — query with words " +
			"that appear in the note line — and are erased line by line (kind memory_note). Check the list against " +
			"what the person asked for; only if it matches, call again with just the preview_token to erase " +
			"exactly those records. Irreversible. You can erase only what you wrote and can read, and the " +
			"notes of the person you act for. Tell the person something was erased ONLY when a result says " +
			"\"erased\": true, quoting its erased_count and request_id; when a result says \"erased\": false, " +
			"follow its tell_the_person and never say it was erased or removed.",
		Schema: []byte(toolSchema),
	}
}

type toolArgs struct {
	Query        string   `json:"query"`
	DocumentIDs  []string `json:"document_ids"`
	ChunkIDs     []string `json:"chunk_ids"`
	ThreadIDs    []string `json:"thread_ids"`
	PreviewToken string   `json:"preview_token"`
	ReasonCode   string   `json:"reason_code"`
}

type toolCandidate struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	DocumentID string `json:"document_id,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
}

// Call implements domain.KernelTool. Errors are values the model sees and can correct.
func (t *Tool) Call(ctx context.Context, call domain.KernelToolCall) ([]byte, error) {
	var a toolArgs
	if len(call.ArgsJSON) > 0 {
		if err := json.Unmarshal(call.ArgsJSON, &a); err != nil {
			return nil, fmt.Errorf("forget_memory: invalid arguments: %w", err)
		}
	}
	if call.AgentID == "" {
		return nil, fmt.Errorf("forget_memory: anonymous caller")
	}
	// The authenticated caller, never an argument (INV-5); the read predicate is
	// already on ctx (runKernelTool).
	ctx = domain.WithPrincipal(ctx, domain.AgentPrincipal(call.AgentID))
	sel := domain.ErasureSelector{DocumentIDs: a.DocumentIDs, ChunkIDs: a.ChunkIDs, ThreadIDs: a.ThreadIDs}

	query := strings.TrimSpace(a.Query)
	if a.PreviewToken == "" {
		var (
			p   Preview
			err error
		)
		switch {
		case query != "" && !sel.IsEmpty():
			return nil, fmt.Errorf("forget_memory: pass either a query or ids to preview, not both")
		case query != "":
			p, err = t.svc.PreviewQuery(ctx, query, call.AgentID)
		default:
			p, err = t.svc.Preview(ctx, sel)
		}
		if errors.Is(err, domain.ErrNothingToErase) {
			return nothingFound()
		}
		if err != nil {
			return nil, fmt.Errorf("forget_memory preview: %w", err)
		}
		cands := make([]toolCandidate, 0, len(p.Candidates))
		for _, c := range p.Candidates {
			cands = append(cands, toolCandidate{Kind: c.Kind, ID: c.ID, DocumentID: c.DocumentID, Snippet: p.Snippets[SnippetKey(c)]})
		}
		return json.Marshal(map[string]any{
			"erased":          false,
			"outcome":         "preview",
			"would_erase":     cands,
			"truncated":       p.Truncated,
			"preview_token":   p.Token,
			"expires_at":      p.ExpiresAt.UTC().Format(time.RFC3339),
			"next":            "nothing was erased; to erase exactly these records call forget_memory with only this preview_token",
			"tell_the_person": "Nothing has been erased yet. Show them what would be erased and erase it only if it is what they asked for.",
		})
	}
	if !sel.IsEmpty() || query != "" {
		return nil, fmt.Errorf("forget_memory: pass either a query or ids (to preview) or preview_token (to erase), not both")
	}
	reason, err := domain.ParseErasureReason(a.ReasonCode)
	if err != nil {
		return nil, fmt.Errorf("forget_memory: %w", err)
	}
	rec, err := t.svc.Forget(ctx, Request{PreviewToken: a.PreviewToken, Reason: reason})
	if errors.Is(err, domain.ErrNothingToErase) {
		return nothingFound()
	}
	if err != nil {
		return nil, fmt.Errorf("forget_memory: %w", err)
	}
	return receiptResult(rec)
}

// The answer shape (ENG-2106, owner decision 2026-11-18): the agent may report an
// erasure only from the receipt, so every result carries "erased" computed HERE from
// the receipt — true only when this call erased at least one record (erased_count > 0,
// not a replay) — and a tell_the_person line. "Nothing to erase" is a result, not an
// error string: an error invited the model to paraphrase it into "it has been
// removed" (readiness ENG-1995 phase B).

func nothingFound() ([]byte, error) {
	return json.Marshal(map[string]any{
		"erased":          false,
		"found":           false,
		"erased_count":    0,
		"outcome":         "nothing_found",
		"tell_the_person": "Nothing matching that was found in the memory you can erase, so nothing was erased. Say exactly that; do not say it was erased or removed.",
	})
}

func receiptResult(rec domain.ErasureReceipt) ([]byte, error) {
	out := map[string]any{
		"request_id": rec.RequestID,
		"status":     rec.Status,
		"targets":    rec.Targets,
		"counts":     rec.Counts,
		"replayed":   rec.Replayed,
	}
	switch {
	case rec.Replayed:
		out["erased"], out["erased_count"], out["outcome"] = false, 0, "already_erased"
		out["tell_the_person"] = fmt.Sprintf("This was already erased by an earlier request (receipt %s); nothing more was erased now.", rec.RequestID)
	case rec.Targets > 0:
		out["erased"], out["erased_count"], out["outcome"] = true, rec.Targets, "erased"
		out["tell_the_person"] = fmt.Sprintf("Erased %d record(s) permanently (receipt %s).", rec.Targets, rec.RequestID)
	default:
		out["erased"], out["erased_count"], out["outcome"] = false, 0, "nothing_erased"
		out["tell_the_person"] = "Nothing was erased. Say exactly that; do not say it was erased or removed."
	}
	return json.Marshal(out)
}
