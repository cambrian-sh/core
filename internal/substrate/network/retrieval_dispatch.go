package network

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

const (
	retrievalPlanTokenLimit = 2048
	retrievalSessionTTL     = 30 * time.Second
)

// RetrievalDispatcher implements memory.Planner (AGENTIC_RETRIEVAL_SPEC §2.1) by
// invoking the privileged retrieval_agent directly via the Auctioneer (no
// auction), exactly like the scout dispatcher. The Go loop owns control; this is
// just dispatch + JSON parse.
//
// Like the Scout, the retrieval_agent is a CognitiveAgent that makes managed LLM
// calls, so a session must be acquired here and injected into the handoff as
// `_session_token_id` — the CallAgent path does not pre-allocate a model the way
// the normal step path does, and without a token the agent's substrate.generate
// fails "session not found".
//
// FAIL-OPEN: any failure (nil auctioneer, dispatch error, empty/bad response)
// returns ("", nil) or ("", err); memory.QueryService.planQuery treats both as
// "use the original query", so recall is never worse than the single pass.
type RetrievalDispatcher struct {
	Auctioneer domain.Auctioneer
	AgentID    string // default "retrieval_agent"
	Gateway    LLMGateway
	Model      string // planner model id; "" ⇒ gateway default
}

var _ memory.Planner = (*RetrievalDispatcher)(nil)

type planStepRequest struct {
	Op         string   `json:"op"`
	Query      string   `json:"query"`
	Scratchpad string   `json:"scratchpad,omitempty"`
	History    []string `json:"history,omitempty"` // ReAct: entities resolved in prior hops (hop-aware relation selection)
	Hop        int      `json:"hop"`               // 0-based hop index; >0 ⇒ later-hop relation planning
}

type decideRequest struct {
	Op      string   `json:"op"`
	Query   string   `json:"query"`
	History []string `json:"history,omitempty"` // ReAct trace: entities resolved in prior hops
	Chunks  []string `json:"chunks"`
}

type synthRequest struct {
	Op     string   `json:"op"`
	Query  string   `json:"query"`
	Chunks []string `json:"chunks"`
}

type decomposeRequest struct {
	Op    string `json:"op"`
	Query string `json:"query"`
}

type answerSubqRequest struct {
	Op     string   `json:"op"`
	Subq   string   `json:"subq"`
	Chunks []string `json:"chunks"`
}

// call dispatches one op to the retrieval_agent with a managed LLM session
// (mirrors scout_dispatch: acquire → inject _session_token_id → CallAgent →
// Complete). Returns the agent's raw response bytes, or (nil, err) fail-open.
func (d *RetrievalDispatcher) call(ctx context.Context, reqData []byte) ([]byte, error) {
	if d == nil || d.Auctioneer == nil {
		return nil, nil
	}
	agentID := d.AgentID
	if agentID == "" {
		agentID = "retrieval_agent"
	}
	h := &domain.Handoff{
		FromAgent: "orchestrator",
		ToAgent:   agentID,
		Payload:   &domain.Payload{Type: "retrieval", Data: reqData},
		Context:   map[string]string{"task_id": "agentic-retrieval"},
	}
	// Empty Model ⇒ gateway default via a metered session.
	if d.Gateway != nil {
		sa := domain.StepAllocation{}
		if d.Model != "" {
			sa.Winner = domain.AgentDefinition{ID: d.Model}
		}
		if tokenID, aerr := d.Gateway.Acquire(ctx, sa, retrievalPlanTokenLimit, retrievalSessionTTL); aerr == nil && tokenID != "" {
			h.Context["_session_token_id"] = tokenID
			defer func() { _, _ = d.Gateway.Complete(ctx, tokenID) }()
		}
	}
	resp, err := d.Auctioneer.CallAgent(ctx, agentID, h, "")
	if err != nil || resp == nil || resp.Payload == nil || len(resp.Payload.Data) == 0 {
		slog.DebugContext(ctx, "retrieval dispatch: no response; fail-open", "err", err)
		return nil, err
	}
	return resp.Payload.Data, nil
}

// PlanQuery asks the retrieval_agent to rewrite the query into a discriminative
// lexical search string for this hop. ``scratchpad`` carries the bridge entity
// on later hops (empty on hop 1). Returns "" (fail open) on any failure.
func (d *RetrievalDispatcher) PlanQuery(ctx context.Context, query, scratchpad string, history []string, hop int) (string, error) {
	reqData, err := json.Marshal(planStepRequest{Op: "plan_step", Query: query, Scratchpad: scratchpad, History: history, Hop: hop})
	if err != nil {
		return "", err
	}
	data, err := d.call(ctx, reqData)
	if err != nil || len(data) == 0 {
		return "", err
	}
	obj := domain.ExtractJSONObject(string(data)) // defensive (prose/fences)
	if obj == "" {
		return "", nil
	}
	var ps struct {
		LexicalQuery string `json:"lexical_query"`
	}
	if err := json.Unmarshal([]byte(obj), &ps); err != nil {
		slog.WarnContext(ctx, "retrieval dispatch: bad plan JSON; fail-open", "err", err)
		return "", err
	}
	slog.InfoContext(ctx, "agentic-trace plan", "scratchpad", scratchpad, "lexical", ps.LexicalQuery)
	return ps.LexicalQuery, nil
}

// ReasonStep (IRCoT) asks the retrieval_agent for the next chain-of-thought
// sentence + whether it reached the answer + the next search query. FAIL-OPEN =
// done (stop the loop) on any error, so a bad step ends with accumulated chunks.
func (d *RetrievalDispatcher) ReasonStep(ctx context.Context, query string, cot, chunks []string) (thought string, done bool, nextQuery string, err error) {
	reqData, merr := json.Marshal(struct {
		Op     string   `json:"op"`
		Query  string   `json:"query"`
		Cot    []string `json:"cot,omitempty"`
		Chunks []string `json:"chunks"`
	}{Op: "reason_step", Query: query, Cot: cot, Chunks: chunks})
	if merr != nil {
		return "", true, "", merr
	}
	data, cerr := d.call(ctx, reqData)
	if cerr != nil || len(data) == 0 {
		return "", true, "", cerr
	}
	obj := domain.ExtractJSONObject(string(data))
	if obj == "" {
		return "", true, "", nil
	}
	var rs struct {
		Thought     string `json:"thought"`
		Done        bool   `json:"done"`
		SearchQuery string `json:"search_query"`
	}
	if uerr := json.Unmarshal([]byte(obj), &rs); uerr != nil {
		slog.WarnContext(ctx, "retrieval dispatch: bad reason JSON; fail-open (done)", "err", uerr)
		return "", true, "", uerr
	}
	slog.InfoContext(ctx, "agentic-trace reason", "done", rs.Done, "thought", rs.Thought, "next", rs.SearchQuery)
	return rs.Thought, rs.Done, strings.TrimSpace(rs.SearchQuery), nil
}

// PlanHyde asks the retrieval_agent for a hypothetical answer passage (HyDE) to
// embed for hop-1 dense retrieval. Returns "" (fail-open ⇒ embed the real query)
// on any failure or empty passage.
func (d *RetrievalDispatcher) PlanHyde(ctx context.Context, query string) (string, error) {
	reqData, err := json.Marshal(struct {
		Op    string `json:"op"`
		Query string `json:"query"`
	}{Op: "hyde", Query: query})
	if err != nil {
		return "", err
	}
	data, err := d.call(ctx, reqData)
	if err != nil || len(data) == 0 {
		return "", err
	}
	obj := domain.ExtractJSONObject(string(data))
	if obj == "" {
		return "", nil
	}
	var hp struct {
		Passage string `json:"passage"`
	}
	if err := json.Unmarshal([]byte(obj), &hp); err != nil {
		slog.WarnContext(ctx, "retrieval dispatch: bad hyde JSON; fail-open", "err", err)
		return "", err
	}
	slog.InfoContext(ctx, "agentic-trace hyde", "passage_len", len(hp.Passage))
	return hp.Passage, nil
}

// DecideContinue asks the retrieval_agent whether the accumulated chunks answer
// the query (stop) or whether a bridge entity must be looked up next (continue).
// FAIL-OPEN = stop: nil auctioneer, dispatch error, bad JSON, or a
// missing/empty bridge all return stop=true, so a bad step ends the loop with
// the results already accumulated rather than spinning.
func (d *RetrievalDispatcher) DecideContinue(ctx context.Context, query string, history, chunks []string) (bool, string, error) {
	reqData, err := json.Marshal(decideRequest{Op: "decide_continue", Query: query, History: history, Chunks: chunks})
	if err != nil {
		return true, "", err
	}
	data, err := d.call(ctx, reqData)
	if err != nil || len(data) == 0 {
		return true, "", err
	}
	obj := domain.ExtractJSONObject(string(data))
	if obj == "" {
		return true, "", nil
	}
	var dr struct {
		Decision     string `json:"decision"`
		Bridge       string `json:"bridge"`
		RawDecision  string `json:"raw_decision"`
		RawBridge    string `json:"raw_bridge"`
		RejectReason string `json:"reject_reason"`
	}
	if err := json.Unmarshal([]byte(obj), &dr); err != nil {
		slog.WarnContext(ctx, "retrieval dispatch: bad decide JSON; fail-open (stop)", "err", err)
		return true, "", err
	}
	// Diagnostic: log the RAW LLM proposal (pre-guard) alongside the applied
	// decision, so we can tell "LLM chose stop" from "guard rejected a bridge".
	slog.InfoContext(ctx, "agentic-trace decide", "decision", dr.Decision, "bridge", dr.Bridge,
		"raw_decision", dr.RawDecision, "raw_bridge", dr.RawBridge, "reject_reason", dr.RejectReason)
	if dr.Decision == "continue" && strings.TrimSpace(dr.Bridge) != "" {
		return false, dr.Bridge, nil
	}
	return true, "", nil
}

// Synthesize asks the retrieval_agent for the final typed output over the
// accumulated chunks. Returns status ∈ {answer, abstention, clarification} + the
// composed text. FAIL-OPEN = "answer": any failure returns ("answer", "", err)
// so a bad synthesis never wrongly abstains on an answerable query.
func (d *RetrievalDispatcher) Synthesize(ctx context.Context, query string, chunks []string) (string, string, error) {
	reqData, err := json.Marshal(synthRequest{Op: "synthesize", Query: query, Chunks: chunks})
	if err != nil {
		return "answer", "", err
	}
	data, err := d.call(ctx, reqData)
	if err != nil || len(data) == 0 {
		return "answer", "", err
	}
	obj := domain.ExtractJSONObject(string(data))
	if obj == "" {
		return "answer", "", nil
	}
	var sr struct {
		Status string `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(obj), &sr); err != nil {
		slog.WarnContext(ctx, "retrieval dispatch: bad synth JSON; fail-open (answer)", "err", err)
		return "answer", "", err
	}
	status := sr.Status
	if status != "abstention" && status != "clarification" {
		status = "answer"
	}
	slog.InfoContext(ctx, "agentic-trace synth", "status", status)
	return status, sr.Text, nil
}

// SynthesizeCited (ADR-0081) is Synthesize with inline [n] citations: the chunks
// are numbered [1..N] in the order given and the answer cites them. Same envelope
// and fail-open as Synthesize. Distinct op so the benchmark synthesize is never
// polluted with markers.
func (d *RetrievalDispatcher) SynthesizeCited(ctx context.Context, query string, chunks []string) (string, string, error) {
	reqData, err := json.Marshal(synthRequest{Op: "synthesize_cited", Query: query, Chunks: chunks})
	if err != nil {
		return "answer", "", err
	}
	data, err := d.call(ctx, reqData)
	if err != nil || len(data) == 0 {
		return "answer", "", err
	}
	obj := domain.ExtractJSONObject(string(data))
	if obj == "" {
		return "answer", "", nil
	}
	var sr struct {
		Status string `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(obj), &sr); err != nil {
		slog.WarnContext(ctx, "retrieval dispatch: bad cited-synth JSON; fail-open (answer)", "err", err)
		return "answer", "", err
	}
	status := sr.Status
	if status != "abstention" && status != "clarification" {
		status = "answer"
	}
	return status, sr.Text, nil
}

// Decompose asks the retrieval_agent to split a multi-hop question into ordered
// sub-questions (with {n} placeholders for earlier answers). Returns nil (⇒ the
// loop falls back to a single pass) on any failure.
func (d *RetrievalDispatcher) Decompose(ctx context.Context, question string) ([]string, []string, error) {
	reqData, err := json.Marshal(decomposeRequest{Op: "decompose", Query: question})
	if err != nil {
		return nil, nil, err
	}
	data, err := d.call(ctx, reqData)
	if err != nil || len(data) == 0 {
		return nil, nil, err
	}
	obj := domain.ExtractJSONObject(string(data))
	if obj == "" {
		return nil, nil, nil
	}
	var dr struct {
		SubQuestions []string `json:"sub_questions"`
		Refs         []string `json:"refs"`
	}
	if err := json.Unmarshal([]byte(obj), &dr); err != nil {
		slog.WarnContext(ctx, "retrieval dispatch: bad decompose JSON; fail-open", "err", err)
		return nil, nil, err
	}
	slog.InfoContext(ctx, "agentic-trace decompose", "sub_questions", dr.SubQuestions, "refs", dr.Refs)
	return dr.SubQuestions, dr.Refs, nil
}

// AnswerSubQuestion asks the retrieval_agent to extract the GROUNDED answer to one
// sub-question from its retrieved chunks. Returns "" (⇒ substitute nothing) on any
// failure or an ungrounded/absent answer.
func (d *RetrievalDispatcher) AnswerSubQuestion(ctx context.Context, subq string, chunks []string) (string, error) {
	reqData, err := json.Marshal(answerSubqRequest{Op: "answer_subq", Subq: subq, Chunks: chunks})
	if err != nil {
		return "", err
	}
	data, err := d.call(ctx, reqData)
	if err != nil || len(data) == 0 {
		return "", err
	}
	obj := domain.ExtractJSONObject(string(data))
	if obj == "" {
		return "", nil
	}
	var ar struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal([]byte(obj), &ar); err != nil {
		return "", err
	}
	slog.InfoContext(ctx, "agentic-trace subq", "subq", subq, "answer", ar.Answer)
	return ar.Answer, nil
}
