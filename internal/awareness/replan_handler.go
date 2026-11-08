package awareness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/prompts"
)

// ── ReplanHandler prompts ────────────────────────────────────────────────────
//
// Library prompts (ADR-0139 C4/D2): prompts/replan and prompts/replan_cap are the
// stable system blocks of the two arms; prompts/replan_input is the user message,
// whose tagged regions carry the capability vocabulary (contract arm only), the
// partial results, the budget marker and, last, the failure. The failed step's
// query and error are user- and tool-derived, so they are data in <failure>, never
// rule text. The hash is the pair's static hash, as for the planner.

var (
	replanPrompts    = prompts.MustPair("replan", "replan_input")
	replanCapPrompts = prompts.MustPair("replan_cap", "replan_input")

	replanPromptHash    = replanPrompts.StaticHash()
	replanPromptHashCap = replanCapPrompts.StaticHash()
)

// replanDataTags are the data regions of replan_input.
var replanDataTags = []string{"capability_clusters", "today", "partial_results", "failure"}

// replanCacheTTL: a replan runs inside DAG execution, daemon-paced and in bursts
// minutes apart at most, so the short (5 minute) breakpoint lifetime is the right
// one (ADR-0139 D1); a 1h write would cost more than it saves.
const replanCacheTTL = domain.CacheTTLShort

// budgetExceededMarker is the replan_input budget value when the plan ran over
// its cost budget; the system block says what it means.
const budgetExceededMarker = "<budget_exceeded/>"

func init() {
	registerPairPrompt("planner.replan", replanPrompts)
	registerPairPrompt("planner.replan.capability_contract", replanCapPrompts)
}

// ── End ReplanHandler prompts ────────────────────────────────────────────────

// CapabilityContractSource reports whether the ROUTE-03 capability_contract arm
// is on and, if so, the vocabulary block and set to plan against. *Planner
// implements it; the replan handler uses it so a replan under the contract arm
// keeps required_capabilities instead of silently dropping them.
type CapabilityContractSource interface {
	CapabilityContract(ctx context.Context) (block string, vocab map[string]struct{}, canonical bool, on bool)
}

// PlannerReplanHandler wraps the Planner to implement substrate.ReplanHandler.
// It sends the replan prompt pair with the failure and the partial results as
// data, through the planner's generator (C1 when it is a RequestGenerator), and
// decodes the schema-validated reply into a new ExecutionPlan for the remaining
// work.
type PlannerReplanHandler struct {
	planner Generator
	caps    CapabilityContractSource // nil ⇒ the default (no capability contract) arm
	// calendar dates the replan prompt's <today> region — the planner's calendar, so
	// a replan resolves "the last two weeks" against the date the plan was made for.
	// nil ⇒ the wall clock in UTC.
	calendar *domain.Calendar
}

// ReplanOption configures a PlannerReplanHandler.
type ReplanOption func(*PlannerReplanHandler)

// WithCapabilityContract makes the handler follow the planner's
// capability_contract arm. src may be nil (no-op).
func WithCapabilityContract(src CapabilityContractSource) ReplanOption {
	return func(h *PlannerReplanHandler) { h.caps = src }
}

// WithCalendar dates the replan prompt with the kernel's calendar (nil ⇒ the wall
// clock in UTC).
func WithCalendar(c *domain.Calendar) ReplanOption {
	return func(h *PlannerReplanHandler) { h.calendar = c }
}

// NewPlannerReplanHandler creates a replan handler backed by any Generator.
func NewPlannerReplanHandler(planner Generator, opts ...ReplanOption) *PlannerReplanHandler {
	h := &PlannerReplanHandler{planner: planner}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Replan calls the Planner with error context and partial results to produce
// a revised ExecutionPlan for the remaining work. Returns nil if replanning
// fails or if the Planner cannot produce a valid plan.
func (h *PlannerReplanHandler) Replan(
	ctx context.Context,
	failedStep int,
	err error,
	partialContext map[string]string,
	originalPlan *domain.ExecutionPlan,
) (*domain.ExecutionPlan, error) {
	errMsg := err.Error()
	failedStepQuery := ""
	if failedStep >= 0 && failedStep < len(originalPlan.Steps) {
		failedStepQuery = originalPlan.Steps[failedStep].Query
	}

	// Partial results in key order: the map's iteration order is random, and a
	// prompt that changes byte order per call is neither reproducible nor cacheable.
	keys := make([]string, 0, len(partialContext))
	for k := range partialContext {
		if strings.HasPrefix(k, "step_") && strings.HasSuffix(k, "_result") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var partialResults strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&partialResults, "- %s: %s\n", k, partialContext[k])
	}

	budget := ""
	var budgetErr *domain.BudgetExceededError
	if errors.As(err, &budgetErr) {
		budget = budgetExceededMarker
	}

	pair := replanPrompts
	promptVersion := replanPromptHash
	var capBlock string
	var capVocab map[string]struct{}
	var canonical bool
	if h.caps != nil {
		var on bool
		capBlock, capVocab, canonical, on = h.caps.CapabilityContract(ctx)
		if on {
			pair = replanCapPrompts
			promptVersion = replanPromptHashCap
		}
	}
	// Effect-checked dispatch: a replan under the contract arm keeps the effect
	// contract (replan_cap v3) — the same block and vocabulary the planner shows.
	var effectVocab *domain.StepEffectVocabulary
	if es, ok := h.caps.(EffectContractSource); ok && pair.System.ID == replanCapPrompts.System.ID {
		var effBlock string
		effBlock, effectVocab = es.EffectContract(ctx)
		capBlock = appendBlock(capBlock, effBlock)
	}

	failure := fmt.Sprintf("Original subject: %s\nFailed step: step %d — %s\nError: %s\nSteps completed: %d",
		originalPlan.Subject, failedStep, failedStepQuery, errMsg, len(keys))
	req, rerr := planRequest(pair, replanDataTags, map[string]string{
		"capability_clusters": capBlock, // empty on the default arm
		"today":               h.calendar.Block(),
		"partial_results":     partialResults.String(),
		"budget":              budget,
		"failure":             failure,
	}, replanCacheTTL)
	if rerr != nil {
		return nil, rerr
	}
	if err := applyEffectSchema(&req, effectVocab); err != nil {
		return nil, fmt.Errorf("replan: %w", err)
	}

	slog.Info("🔄 Replanning", "failed_step", failedStep, "error", errMsg)

	resp, genErr := domain.GenerateVia(ctx, h.planner, req)
	if genErr != nil {
		slog.Error("ReplanHandler: planner call failed", "error", genErr)
		return nil, genErr
	}

	var reply planReply
	if derr := decodePlanReply(resp, &reply); derr != nil {
		slog.Error("ReplanHandler: reply is not a plan", "error", derr)
		return nil, fmt.Errorf("replan: %w", derr)
	}
	if reply.Reasoning != "" {
		slog.Debug("replan reasoning", "reasoning", reply.Reasoning)
	}
	plan := reply.ExecutionPlan

	// PROMPTREQ: record the replan prompt hash so PlanEvent.PlannerPromptVersion
	// reflects the last accepted Generate() call.
	plan.PlannerPromptVersion = promptVersion

	// Same ROUTE-03 vocabulary guard as the planner: a replanned step must not be
	// able to hard-fail on a capability tag the model was not shown.
	dropInventedCapabilities(ctx, &plan, capVocab, canonical)
	normalizeDeclaredEffects(ctx, &plan, effectVocab)

	slog.Info("✅ Replan produced", "steps", len(plan.Steps), "subject", plan.Subject)
	return &plan, nil
}
