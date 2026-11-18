package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	mcp "github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/sandbox"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// buildOperatorSurface binds the operator command effects to kernel surfaces
// (ADR-0047 0047-16/0047-25), builds the Authenticator (ADR-0142 D1), and the
// share-to-team publish effect (ADR-0142 D12).
func (b *kernelBuild) buildOperatorSurface() error {
	ctx, cfg, opts := b.ctx, b.cfg, b.opts
	vec, mem, docLifecycle, eventBus, idStores, authorizer, toolApproval := b.vec, b.mem, b.docLifecycle, b.eventBus, b.idStores, b.authorizer, b.toolApproval
	skillReg, skillIndexer, mcpConnector, sbPolicy, mcpSink := b.skillReg, b.skillIndexer, b.mcpConnector, b.sbPolicy, b.mcpSink
	// ADR-0047 0047-16/0047-25: real CommandEffects bound to kernel surfaces.
	// TagMemory follows Amendment A1.2: operators may widen AND narrow tags, from
	// the controlled vocabulary, written through the kernel store (not a raw DB
	// write), audited by the command path.
	// The controlled vocabulary lives with the decision point. With no policy
	// plugin installed there is no vocabulary to violate, so any tag is accepted —
	// consistent with the rest of the OSS posture.
	operatorEffects := operator.CommandEffectsFuncs{
		TagMemoryFn: func(ctx context.Context, docID, tag string, add bool) error {
			// ADR-0146 D5: owner tags are kernel-stamped. Adding one would forge
			// "written for <id>" (which the own-rows rule trusts); removing one would
			// lift a person's owner fence. RetagDocument already preserves them on the
			// document path; the memory-row path below would not, so refuse here.
			if domain.IsOwnerTag(tag) {
				return fmt.Errorf("tag_memory: %q is an owner tag; owner tags are stamped by the kernel and cannot be added or removed", tag)
			}
			if opts.PolicyAdmin != nil && !opts.PolicyAdmin.ValidateTag(tag) {
				return fmt.Errorf("tag %q is not in the controlled vocabulary", tag)
			}
			// ADR-0093: try the DOCUMENT ENTITY first. `documents.tags` is the
			// authoritative classification, and the per-chunk copies are a derived
			// cache — so tagging a document has to go through RetagDocument, which
			// moves the row and every one of its chunks in one transaction.
			//
			// Writing the chunk directly (which is all this used to do) left the
			// document row saying one thing and its chunks another: precisely the
			// half-classified state the split was built to make impossible. It also
			// could not tag a document at ALL, because GetByID searches the chunk and
			// descriptor tables and `documents` is deliberately not among them.
			if current, found, derr := vec.DocumentTags(ctx, docID); derr != nil {
				return fmt.Errorf("tag_memory: %w", derr)
			} else if found {
				return docLifecycle.RetagDocument(ctx, docID, applyTag(current, tag, add))
			}

			// Retrieval returns CHUNKS, so an operator labelling a search result holds a
			// chunk id. Label its parent document instead: tagging the chunk alone would
			// leave the document's other chunks unlabelled, which is the half-classified
			// state this design exists to prevent — produced by the feature meant to fix
			// classification.
			if parent, ok, perr := vec.ParentDocumentOf(ctx, docID); perr != nil {
				return fmt.Errorf("tag_memory: %w", perr)
			} else if ok {
				current, _, terr := vec.DocumentTags(ctx, parent)
				if terr != nil {
					return fmt.Errorf("tag_memory: %w", terr)
				}
				return docLifecycle.RetagDocument(ctx, parent, applyTag(current, tag, add))
			}

			// Not a document and not part of one — an agent-written memory or a tool
			// descriptor. Those have no parent to be authoritative, so the row itself
			// is it.
			// Those have no parent to be authoritative, so the row itself is it.
			doc, err := vec.GetByID(ctx, docID)
			if err != nil || doc == nil {
				return fmt.Errorf("tag_memory: %s is neither a document nor a memory: %w", docID, err)
			}
			tags := stringSliceFromMeta(doc.Metadata["tags"])
			tags = applyTag(tags, tag, add)
			if doc.Metadata == nil {
				doc.Metadata = map[string]interface{}{}
			}
			doc.Metadata["tags"] = tags
			return vec.Save(ctx, doc)
		},
		SetScopeFn: func(ctx context.Context, agentID string, required, anyOf, forbidden []string) error {
			if opts.PolicyAdmin == nil {
				return fmt.Errorf("set_scope requires the access-policy plugin; this build has none")
			}
			return opts.PolicyAdmin.SetAgentScope(ctx, agentID, required, anyOf, forbidden)
		},
		RegisterSkillFn: func(ctx context.Context, name, description, instructions string, toolGrants, scopeTags []string) error {
			sk := domain.Skill{Name: name, Description: description, Instructions: instructions, ToolGrants: toolGrants, ScopeTags: scopeTags}
			skillReg.Register(sk)
			return skillIndexer.Index(ctx, sk)
		},
		RegisterMCPFn: func(ctx context.Context, name, command, url string) error {
			if mcpConnector == nil {
				return fmt.Errorf("mcp connector not enabled")
			}
			// A server registered at runtime by a tool call is unknown code:
			// the default tier (third-party unless configured otherwise).
			cfg := mcp.ServerConfig{ID: name, Transport: "stdio", Endpoint: command,
				Sandbox: sbPolicy.Resolve(name, "", "", false, sandbox.ServerSpec{})}
			if url != "" {
				cfg.Transport, cfg.Endpoint = "http", url
			}
			tools := mcpConnector.ConnectAll(ctx, []mcp.ServerConfig{cfg})
			if mcpSink != nil {
				mcpSink.SetServerTools(ctx, name, tools) // registers + indexes (ADR-0044)
			}
			return nil
		},
		TriggerConsolidationFn: func(ctx context.Context, scope string) error {
			return eventBus.Publish(ctx, domain.MemoryPressureEvent{Trigger: "operator:" + scope})
		},
		// ADR-0054 tuning seam: hot-apply Stage-A blend weights live (no restart).
		// Merges the provided params over the current live weights; unknown keys are
		// logged + ignored. Ephemeral — config.json remains the boot default.
		SetRuntimeConfigFn: func(_ context.Context, params map[string]float64) error {
			w := mem.QueryService.CurrentBlendWeights()
			applied := 0
			for k, v := range params {
				switch k {
				case "blend_weight_cosine":
					w.Cosine = v
				case "blend_weight_lexical":
					w.Lexical = v
				case "blend_weight_coherence":
					w.GraphCoherence = v
				case "blend_weight_confidence":
					w.Confidence = v
				case "blend_weight_pagerank":
					w.PageRank = v
				case "blend_weight_recency":
					w.Recency = v
				case "blend_weight_activation":
					w.Activation = v
				default:
					slog.Warn("SetRuntimeConfig: unknown tunable ignored", "key", k, "value", v)
					continue
				}
				applied++
			}
			if applied == 0 {
				return fmt.Errorf("no known tunables in params (supported: blend_weight_{cosine,lexical,coherence,confidence,pagerank,recency,activation})")
			}
			mem.QueryService.SetBlendWeights(w)
			slog.Info("SetRuntimeConfig: blend weights hot-applied (ephemeral)", "weights", w, "applied", applied)
			return nil
		},
	}

	authenticator, err := buildAuthenticator(ctx, cfg, idStores, opts.NewAuthenticator)
	if err != nil {
		return err
	}

	// ADR-0142 D12: the explicit publish. The raw adapter (tier 0) for the retag —
	// a kernel-authored write after the owner, policy and approval checks in
	// memorySharer.plan and operator.ShareToTeam, the TagMemory lane.
	sharer := &memorySharer{docs: retagThroughLifecycle{vec, docLifecycle}, authz: authorizer}
	toolApproval.HandleKind(domain.ApprovalKindPublish, publishHandler(sharer))

	b.operatorEffects, b.authenticator, b.sharer = operatorEffects, authenticator, sharer
	return nil
}

// stringSliceFromMeta coerces a document metadata value into a []string (tags
// may round-trip through JSON as []interface{}). ADR-0047 0047-25.
func stringSliceFromMeta(v interface{}) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// applyTag adds or removes a tag from a set, de-duplicating. ADR-0047 A1.2.
func applyTag(tags []string, tag string, add bool) []string {
	out := make([]string, 0, len(tags)+1)
	found := false
	for _, t := range tags {
		if t == tag {
			found = true
			if !add {
				continue // remove
			}
		}
		out = append(out, t)
	}
	if add && !found {
		out = append(out, tag)
	}
	return out
}
