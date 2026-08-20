package ingress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// Chat capture — an admitted message becomes evidence with identity.
//
// FIVE-PLANES-BUILD seam S7 recorded what this file exists to end: the chat lane
// wrote NO evidence at all. A message arrived, was authorised, ran a turn, was
// answered, and left nothing behind that any other plane could cite. Everything
// else that enters this deployment — a webhook, a poll, a file — is preserved as
// evidence first and projected afterwards; chat was the one entry point whose
// traffic existed only as a transcript row, so a question like "what did the
// customer actually tell us, and where is the record" had no answer, and the
// identity plane (five-planes steps 1+2) could not see a conversation at all.
//
// The same seam recorded the second half of the loss: admission RESOLVED the
// sender's identity binding and then discarded it. The one moment the system
// knows who is speaking is the moment before the turn runs, and that fact was
// being thrown away rather than written down.
//
// Two rules shape everything below:
//
//   - Capture goes through the SAME chokepoint as every other lane
//     (evidence.Ingestor.Ingest, ADR-0105's bytes → verify → atomic commit).
//     A second write path into the archive is a second set of guarantees to
//     keep in step, and they would not stay in step.
//   - Capture NEVER fails a turn. Chat is the one lane with a person waiting on
//     the other side, so an archive that is down, misconfigured or simply not
//     enabled must cost a log line, not a reply. Every error here is logged and
//     swallowed deliberately; the turn proceeds with an empty evidence id.
// ─────────────────────────────────────────────────────────────────────────────

// EvidenceIngestFunc is the kernel's evidence write chokepoint, in the shape
// app.KernelServices.EvidenceIngest already publishes it. Taken as a function
// rather than the concrete *evidence.Ingestor so this package keeps its inward
// dependency on domain alone.
type EvidenceIngestFunc func(ctx context.Context, raw domain.RawEvidence) (domain.EvidenceID, bool, error)

const (
	// ChatSourceIDPrefix namespaces every chat message's evidence SourceID, the
	// way the raw-delivery lane's "ingress:" prefix namespaces a transport
	// delivery: the SourceID is a CLAIM KEY, so a transformer can one day say
	// "these rows are mine" without inspecting content.
	ChatSourceIDPrefix = "chat:"

	// ChatSurfaceTagPrefix builds the surface tag every captured message
	// carries. It mirrors the raw-delivery lane's convention (premium
	// ingressstudio.SurfaceTag renders the same "ingress:<id>" string) — the
	// INTENT is copied, not the code, because a chat message never passes
	// through the studio and must still land in the same grantable surface an
	// operator already reasons about. A record with no surface tag is the worst
	// of both worlds: it leaks to a policy that grants broadly and hides from
	// one that grants by surface.
	ChatSurfaceTagPrefix = "ingress:"

	// ChatMessageEventType is the occurrence a captured message becomes. DATA,
	// not a branch: nothing in the kernel tests for this string, and event types
	// are unvalidated by precedent (seam S3), so no registry declaration is
	// required or wanted.
	ChatMessageEventType = "chat_message"

	// The two roles a message has participants in. Author is WHO SPOKE; thread
	// is WHERE. Roles rather than payload fields because a role is a real edge:
	// "every event this person took part in" is a query over event_roles, and a
	// sender id buried in a JSON body is not.
	ChatRoleAuthor = "author"
	ChatRoleThread = "thread"

	// Entity id prefixes for those two roles. The stem before "/" is the entity
	// KIND (amendment S3), so both are lowercase snake and both are minted the
	// same way every other entity is.
	ChatUserEntityPrefix   = "chat_user/"
	ChatThreadEntityPrefix = "thread/"

	// defaultCaptureNamespace is the namespace captured chat lands in.
	//
	// A core ingress registration carries no namespace id of its own — its
	// `Namespace` field is the set of external-id PREFIXES it may speak for, a
	// different thing entirely — so there is nothing on the delivery to read one
	// from. "default" is what every other core read path resolves against
	// (postgres/query_plane.go), which makes it the only value under which a
	// captured message is findable. A deployment that grows real namespaces sets
	// ChatCapture.Namespace and this constant stops being load-bearing.
	defaultCaptureNamespace = "default"

	// sourceKeyDigestLen is how much of the message digest rides in the source
	// key. 24 hex characters is 96 bits — a collision inside one conversation is
	// not a thing that happens, and a full 64-character key makes every log line
	// and every operator's eye slide off it.
	sourceKeyDigestLen = 24
)

// ChatCapture preserves admitted chat messages and their identity.
//
// Constructed once and shared: it holds ports, no per-message state.
type ChatCapture struct {
	ingest EvidenceIngestFunc
	events domain.EventStore
	// entities mints the author and thread rows so the identity plane can
	// answer `entity chat_user/tg:12345` rather than only reaching them through
	// an event's roles. Optional: the roles are written either way, and a
	// deployment without the identity plane loses discoverability, not capture.
	entities domain.EntityStore

	// Namespace is where captured chat lands. Empty means defaultCaptureNamespace.
	Namespace string
	// Floor is the deployment's classification floor for chat traffic, added to
	// every captured message beneath the surface tag.
	//
	// Empty is the honest default and the reason it is a field rather than a
	// constant: a studio ingress declares its floor in its transport spec, and a
	// chat ingress has no spec to declare one in. Until an operator sets one,
	// the surface tag IS the classification — which is a real statement (a
	// surface-scoped policy can grant it) rather than the untagged row that
	// leaks and hides at once.
	Floor []string

	logger *slog.Logger
	now    func() time.Time
}

// NewChatCapture wires the capture lane. Both ports may be nil, which is how a
// deployment with evidence capture disabled behaves: Capture becomes a no-op
// that returns an empty id, and the turn is untouched.
func NewChatCapture(ingest EvidenceIngestFunc, events domain.EventStore) *ChatCapture {
	return &ChatCapture{
		ingest: ingest,
		events: events,
		logger: slog.Default(),
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// SetEntityStore wires the optional identity-plane mint (see the field comment).
func (c *ChatCapture) SetEntityStore(s domain.EntityStore) {
	if c != nil {
		c.entities = s
	}
}

// SetLogger overrides the default logger.
func (c *ChatCapture) SetLogger(l *slog.Logger) {
	if c != nil && l != nil {
		c.logger = l
	}
}

// capturedMessage is the BODY that gets archived — what arrived, in the shape it
// arrived in, and nothing derived.
//
// Deliberately free of any wall clock. The archive's idempotency triple is
// (namespace, source_id, source_key, source_revision) and the revision is this
// body's digest, so a timestamp inside it would make every redelivery a new
// revision of the same message: the archive would grow a copy per retry and the
// dedup that makes replay safe everywhere else would be silently absent here.
// When the message arrived is a column (SourceTime), not a byte.
type capturedMessage struct {
	Conversation string `json:"conversation"`
	Ingress      string `json:"ingress"`
	Surface      string `json:"surface,omitempty"`
	ExternalID   string `json:"external_id"`
	SpeakerID    string `json:"speaker_id,omitempty"`
	Username     string `json:"username,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	Text         string `json:"text"`
}

// chatCaptureInput is everything admission knows at the moment it knows it.
type chatCaptureInput struct {
	conversationID string
	addr           domain.DeliveryAddress
	surface        string
	msg            InboundMessage
	// parties are the entity identities this message is ABOUT (ADR-0121) —
	// derived from the resolved identity binding, see chatParties.
	parties []string
}

// Capture archives one admitted message and records who said it, where.
//
// Returns the evidence id, or "" when nothing was written. The caller MUST NOT
// treat "" as a failure to act on: it is the ordinary answer in a deployment
// with evidence capture disabled, and the deliberate answer when the archive
// refused. Nothing here ever returns an error, and that is the point — see the
// availability rule at the top of the file.
func (c *ChatCapture) Capture(ctx context.Context, in chatCaptureInput) domain.EvidenceID {
	if c == nil || c.ingest == nil {
		return ""
	}
	body, err := json.Marshal(capturedMessage{
		Conversation: in.conversationID,
		Ingress:      in.addr.IngressAgentID,
		Surface:      in.surface,
		ExternalID:   in.addr.ExternalID,
		SpeakerID:    in.msg.SpeakerID,
		Username:     in.msg.Username,
		DisplayName:  in.msg.DisplayName,
		Text:         in.msg.Text,
	})
	if err != nil {
		c.logger.Warn("chat capture: the message could not be encoded for the archive; the turn proceeds uncaptured",
			"conversation", in.conversationID, "err", err)
		return ""
	}
	digest := sha256.Sum256(body)
	revision := hex.EncodeToString(digest[:])
	key := chatSourceKey(in.conversationID, revision)

	id, _, err := c.ingest(ctx, domain.RawEvidence{
		NamespaceID:    c.namespace(),
		SourceID:       ChatSourceIDPrefix + in.addr.IngressAgentID,
		SourceKey:      key,
		SourceRevision: revision,
		SourceTime:     c.now(),
		Bytes:          body,
		Classification: c.classification(in.addr.IngressAgentID),
		Parties:        in.parties,
	})
	if err != nil {
		// The whole reason this is a warn and not a return: a person is waiting.
		c.logger.Warn("chat capture: the message was not archived; the turn proceeds uncaptured",
			"conversation", in.conversationID, "ingress", in.addr.IngressAgentID, "err", err)
		return ""
	}

	c.recordEvent(ctx, in, id, key)
	return id
}

// recordEvent writes the one occurrence a message is, with its author and thread
// as real participant edges.
//
// Idempotent on (namespace, source_ref) in the store, and the source ref is the
// same per-message key the evidence row carries — so a redelivered message that
// dedups in the archive also dedups here, and the two planes cannot disagree
// about how many times a person said something.
func (c *ChatCapture) recordEvent(ctx context.Context, in chatCaptureInput, evidenceID domain.EvidenceID, key string) {
	if c.events == nil {
		return
	}
	author := ChatUserEntityPrefix + chatSenderID(in.addr, in.msg)
	thread := ChatThreadEntityPrefix + in.conversationID
	c.mint(ctx, author, evidenceID)
	c.mint(ctx, thread, evidenceID)

	if _, _, err := c.events.RecordEvent(ctx, domain.Event{
		NamespaceID: c.namespace(),
		Type:        ChatMessageEventType,
		OccurredAt:  c.now(),
		EvidenceID:  evidenceID,
		SourceRef:   key,
		Roles: []domain.EventRole{
			{Role: ChatRoleAuthor, EntityID: author},
			{Role: ChatRoleThread, EntityID: thread},
		},
	}); err != nil {
		// The evidence row already landed, so the message is not lost — only its
		// identity edges are, which is a degradation rather than a hole.
		c.logger.Warn("chat capture: the message was archived but its author/thread edges were not recorded",
			"conversation", in.conversationID, "evidence", string(evidenceID), "err", err)
	}
}

// mint records the entity behind a role, idempotently. Best-effort by design:
// the role edge is written whether or not this succeeded, and an unminted entity
// costs discoverability in the `entity` op, never capture.
func (c *ChatCapture) mint(ctx context.Context, id string, evidenceID domain.EvidenceID) {
	if c.entities == nil {
		return
	}
	kind, ok := domain.EntityKindFromID(id)
	if !ok {
		return
	}
	if _, err := c.entities.EnsureEntity(ctx, domain.Entity{
		ID:                id,
		NamespaceID:       c.namespace(),
		Kind:              kind,
		FirstSeenEvidence: evidenceID,
	}); err != nil {
		c.logger.Debug("chat capture: entity not minted; the role edge stands regardless",
			"entity", id, "err", err)
	}
}

func (c *ChatCapture) namespace() string {
	if strings.TrimSpace(c.Namespace) != "" {
		return c.Namespace
	}
	return defaultCaptureNamespace
}

// classification is the captured message's tag set: the surface tag first, then
// the deployment's floor.
//
// One function, for the reason the raw-delivery lane gives for its own: the tags
// reach the row a policy filters on, and no caller may be in a position to omit
// the surface tag. An ingress with no id produces no surface tag rather than a
// bare "ingress:" prefix — one shared tag standing for nothing looks like a
// grantable surface and is not one.
func (c *ChatCapture) classification(ingressID string) []string {
	tags := make([]string, 0, len(c.Floor)+1)
	if ingressID != "" {
		tags = append(tags, ChatSurfaceTagPrefix+ingressID)
	}
	for _, t := range c.Floor {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return nil
	}
	return dedupTags(tags)
}

// dedupTags keeps the set stable and duplicate-free, so the same message
// classified twice produces byte-identical evidence.
func dedupTags(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// chatSourceKey is the per-message identity: the conversation, then a digest of
// the message itself.
//
// DERIVED rather than transported, because nothing on the wire carries a message
// id — the SDK's ingress payload is {external_id, text, …} and the kernel never
// sees Telegram's update id. The consequence is stated plainly rather than
// hidden: two byte-identical messages in the same conversation share one key, so
// a person who types "ok" twice is archived once. That is the same bargain every
// other lane strikes on its idempotency triple, and it is the right side of the
// trade — a duplicate archive row for every transport retry would corrupt any
// count taken over the lane, while a repeated "ok" is recoverable from the
// transcript, which chat (unlike a webhook) always has.
func chatSourceKey(conversationID, revision string) string {
	d := revision
	if len(d) > sourceKeyDigestLen {
		d = d[:sourceKeyDigestLen]
	}
	return conversationID + "/" + d
}

// chatSenderID is the STABLE id of whoever spoke.
//
// SpeakerID first: in a group chat the conversation is the group and the
// external id names the room, so keying the author on the external id would
// merge every member of a room into one person. Falls back to the external id,
// which is what a one-to-one bridge reports and all it reports.
func chatSenderID(addr domain.DeliveryAddress, m InboundMessage) string {
	if s := strings.TrimSpace(m.SpeakerID); s != "" {
		return s
	}
	return addr.ExternalID
}

// chatParties turns the resolved identity binding into the party identities the
// captured evidence is ABOUT (ADR-0121 D2).
//
// This is the fact seam S7 recorded as discarded. Admission resolves the binding
// to decide whether the sender is blocked and then dropped it on the floor —
// the one moment the deployment knows WHO is speaking, spent and forgotten. The
// bound principal is what an operator assigns as a party identity, so writing it
// here is what makes "this person's own conversations" a scope a row-level
// policy can express at all.
//
// An unbound sender is a party to nothing, and that is fail-closed on purpose
// (ADR-0121 D6): "we could not tell who this is" must not read as "everyone".
// Deliberately NOT derived from the sender's raw external id — a party identity
// that any stranger can mint by messaging a bot is one the party registry must
// never have been fed from.
func chatParties(b domain.IdentityBinding, bound bool) []string {
	if !bound || strings.TrimSpace(b.BoundToID) == "" {
		return nil
	}
	return []string{b.BoundToID}
}
