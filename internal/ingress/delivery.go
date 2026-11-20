// Package ingress owns the outbound half of ADR-0090: turning "say this in
// conversation C" into a message on the wire of whichever entry point C arrived
// through.
//
// The inbound half needs no package — a signal from an ingress travels the
// ordinary agent plane, and what makes it an ingress signal is the registration
// the kernel looks up (ADR-0090 D2/D3). Outbound needs one because the address
// resolution, the revocation re-check and the permanent/transient split all have
// to happen in exactly one place.
package ingress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ConversationReader is the narrow slice of the conversation store this needs.
// Deliberately not the whole ConversationStore: delivery reads an address, it
// does not author conversations.
type ConversationReader interface {
	GetConversation(ctx context.Context, id string) (*domain.Conversation, error)
}

// DeliveryService resolves and dispatches outbound messages.
//
// It is the only place that turns a conversation id into a recipient. Everything
// upstream — an agent speaking, a watch firing, an operator broadcasting — names
// a conversation and gets the same resolution, the same revocation check and the
// same failure classification.
type DeliveryService struct {
	convs     ConversationReader
	ingresses domain.IngressResolver
	transport domain.IngressTransport
	journal   domain.DeliveryJournal
	logger    *slog.Logger
	now       func() time.Time

	// Redrive tuning (ADR-0090 D8.1). Exported so the composition root and tests
	// can set them; the defaults are the Default* constants.
	RedriveInterval   time.Duration // how often the redriver runs
	RedriveGrace      time.Duration // a prepared/dispatched intent younger than this is left to its caller
	MaxAttempts       int           // a prepared intent fails after this many dispatches
	DeliveryRetention time.Duration // settled and ambiguous intents older than this are pruned (0 = keep)

	stopMu sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// Redrive defaults (ADR-0090 D8.1).
const (
	DefaultRedriveInterval   = 15 * time.Second
	DefaultRedriveGrace      = 30 * time.Second
	DefaultMaxAttempts       = 8
	DefaultDeliveryRetention = 30 * 24 * time.Hour
	// redriveBatch bounds one pass so a backlog cannot hold the store's write lock.
	redriveBatch = 100
	// maxRedriveBackoff caps the wait between redrives of one intent.
	maxRedriveBackoff = 30 * time.Minute
)

// NewDeliveryService wires the outbound path. A nil journal means best-effort
// delivery: still attempted, but a retry may duplicate and an undeliverable
// message is logged rather than recorded.
func NewDeliveryService(convs ConversationReader, ingresses domain.IngressResolver, transport domain.IngressTransport) *DeliveryService {
	return &DeliveryService{
		convs:     convs,
		ingresses: ingresses,
		transport: transport,
		logger:    slog.Default(),
		now:       time.Now,

		RedriveInterval:   DefaultRedriveInterval,
		RedriveGrace:      DefaultRedriveGrace,
		MaxAttempts:       DefaultMaxAttempts,
		DeliveryRetention: DefaultDeliveryRetention,
	}
}

// SetJournal attaches the two-phase delivery journal (ADR-0090 D8.1). Call it
// before Start; without it delivery is best-effort.
func (s *DeliveryService) SetJournal(j domain.DeliveryJournal) { s.journal = j }

// SetLogger overrides the default logger.
func (s *DeliveryService) SetLogger(l *slog.Logger) {
	if l != nil {
		s.logger = l
	}
}

// Deliver sends text to whoever the conversation belongs to.
//
// txnID makes a retry idempotent; pass the message id, since delivering message M
// is the same act however many times it is attempted. An empty txnID disables the
// idempotency check for this call rather than blocking it — a caller with nothing
// stable to key on should still be able to send.
//
// The order of the checks is the design: resolve, then re-authorise, then send.
// Re-authorising after resolution is what makes revocation effective on
// conversations that were bound while the ingress was still trusted.
// DeliverProgress sends a supersedable progress snapshot (ADR-0098).
//
// It differs from Deliver in three ways, each deliberate:
//
//   - No txnID, so it never touches the idempotency journal. Progress is a snapshot,
//     not an event; "already sent" is meaningless when each update replaces the last.
//   - Errors are returned but callers are expected to drop them. Progress is
//     best-effort by construction (ADR-0098 D5).
//   - A conversation with no delivery address is NOT an error here. Most conversations
//     have no one waiting on the far side, and progress for them is simply moot.
func (s *DeliveryService) DeliverProgress(ctx context.Context, conversationID, text string, final bool) error {
	// An EMPTY text is meaningful here and must not be short-circuited: it is the
	// "clear the status line" signal (ADR-0098 D3). Only a missing transport is a no-op.
	if s.transport == nil {
		return nil
	}
	conv, err := s.convs.GetConversation(ctx, conversationID)
	if err != nil {
		return err
	}
	if conv.Delivery.IsZero() {
		return nil // nobody is waiting on an ingress; nothing to report to
	}
	// Re-authorise on every snapshot, exactly as a real delivery does. Progress is
	// lower-stakes content but it still leaves the deployment, so revoking an ingress
	// must stop it too.
	if err := s.authorise(ctx, conv.Delivery); err != nil {
		return err
	}
	// Per-ingress opt-in (ADR-0098 D6): an ingress whose registration does not
	// ask for progress gets none. A quiet no-op, not an error — a refusal here
	// would read to the progress bridge as a transport pushing back and widen its
	// interval for nothing. With no registry (OSS) nothing is registered, so
	// nothing opted out; the premium plugin's kill switch decides there.
	if !s.progressEnabled(ctx, conv.Delivery) {
		return nil
	}
	return s.transport.Deliver(ctx, domain.IngressDelivery{
		ConversationID: conversationID,
		Address:        conv.Delivery,
		Text:           text,
		Kind:           domain.DeliveryKindProgress,
		Final:          final,
	})
}

func (s *DeliveryService) Deliver(ctx context.Context, conversationID, text, txnID string) error {
	if s.transport == nil {
		return errors.New("delivery: no ingress transport configured")
	}
	if text == "" {
		// An empty delivery would reach the far side as a blank message, which reads
		// as a bug to the person receiving it.
		return errors.New("delivery: text is required")
	}

	conv, err := s.convs.GetConversation(ctx, conversationID)
	if err != nil {
		return err
	}
	if conv.Delivery.IsZero() {
		return fmt.Errorf("%w: %s", domain.ErrNoDeliveryAddress, conversationID)
	}

	// Re-authorise against the CURRENT registration. An address bound yesterday
	// through an ingress that has since been revoked, or whose namespace was
	// narrowed, must not still deliver.
	if err := s.authorise(ctx, conv.Delivery); err != nil {
		return err
	}

	d := domain.IngressDelivery{
		ConversationID: conversationID,
		Address:        conv.Delivery,
		Text:           text,
		TxnID:          txnID,
	}

	if s.journal == nil || txnID == "" {
		// Best-effort: one attempt, nothing recorded.
		if err := s.transport.Deliver(ctx, d); err != nil {
			return s.classify(d, err)
		}
		return nil
	}

	// ADR-0090 D8.1 phase 1: record the intent BEFORE anything leaves the process.
	key := deliveryKey(conversationID, txnID)
	intent, _, err := s.journal.PrepareDelivery(ctx, domain.DeliveryIntent{
		Key:            key,
		ConversationID: conversationID,
		TxnID:          txnID,
		Address:        conv.Delivery,
		Text:           text,
		CreatedAt:      s.now().UTC(),
	})
	if err != nil {
		// A journal that cannot record must not silently become a duplicate (or a
		// lost) sender. Refusing is the safe direction: the caller can retry.
		return fmt.Errorf("delivery: prepare intent: %w", err)
	}
	switch intent.State {
	case domain.DeliveryPrepared:
		return s.dispatch(ctx, intent, d)
	case domain.DeliveryConfirmed:
		return nil // already delivered on an earlier attempt
	case domain.DeliveryFailed:
		return fmt.Errorf("%w: %s", domain.ErrDeliveryPermanent, intent.Err)
	default: // dispatched or ambiguous: an earlier attempt may have landed
		return fmt.Errorf("%w (%s, %s)", domain.ErrDeliveryAmbiguous, key, intent.State)
	}
}

// dispatch runs phases 2 and 3 for one prepared intent: CAS prepared →
// dispatched, send, then settle by what the failure (if any) proves.
func (s *DeliveryService) dispatch(ctx context.Context, intent domain.DeliveryIntent, d domain.IngressDelivery) error {
	if err := s.journal.AdvanceDelivery(ctx, intent.Key, domain.DeliveryPrepared, domain.DeliveryDispatched, ""); err != nil {
		if errors.Is(err, domain.ErrDeliveryStateConflict) {
			// Another attempt (a caller retry or the redriver) owns this send; its
			// outcome is recorded on the intent.
			return nil
		}
		return fmt.Errorf("delivery: mark dispatched: %w", err)
	}
	sendErr := s.transport.Deliver(ctx, d)
	// Settlement must survive the caller's cancellation: the send already happened
	// (or failed), and an intent left dispatched would turn ambiguous.
	sctx := context.WithoutCancel(ctx)
	switch {
	case sendErr == nil:
		if err := s.journal.AdvanceDelivery(sctx, intent.Key, domain.DeliveryDispatched, domain.DeliveryConfirmed, ""); err != nil {
			s.logger.Error("ADR-0090: delivered but the confirmation could not be recorded; the redriver will mark it ambiguous",
				"conversation", d.ConversationID, "key", intent.Key, "err", err)
		}
		return nil
	case errors.Is(sendErr, domain.ErrDeliveryPermanent):
		s.settle(sctx, intent.Key, domain.DeliveryFailed, sendErr)
		s.logger.Warn("ADR-0090: permanently undeliverable; recorded as failed",
			"conversation", d.ConversationID, "address", d.Address.String(), "reason", sendErr)
		return sendErr
	case errors.Is(sendErr, domain.ErrDeliveryTransient):
		// Positive evidence nothing was sent: back to prepared, the redriver retries.
		s.settle(sctx, intent.Key, domain.DeliveryPrepared, sendErr)
		return sendErr
	default:
		// The message may have left the process. Never resend it blindly.
		s.settle(sctx, intent.Key, domain.DeliveryAmbiguous, sendErr)
		s.logger.Error("ADR-0090: delivery outcome unknown; recorded as ambiguous and NOT resent",
			"conversation", d.ConversationID, "address", d.Address.String(), "key", intent.Key, "err", sendErr)
		return fmt.Errorf("%w: %v", domain.ErrDeliveryAmbiguous, sendErr)
	}
}

// ListIntents returns the journal's intents in any of states (none ⇒ ambiguous and
// failed: the ones a person may need to look at), least recently updated first, at
// most limit (ENG-1657).
func (s *DeliveryService) ListIntents(ctx context.Context, states []domain.DeliveryIntentState, limit int) ([]domain.DeliveryIntent, error) {
	if s.journal == nil {
		return nil, domain.ErrDeliveryJournalOff
	}
	if len(states) == 0 {
		states = []domain.DeliveryIntentState{domain.DeliveryAmbiguous, domain.DeliveryFailed}
	}
	return s.journal.ListDeliveries(ctx, states, limit)
}

// ResolveAmbiguous settles one AMBIGUOUS intent on a person's word (ENG-1657, owner
// decision 2026-11-20, option A). The kernel never resends an ambiguous message by
// itself (it may already have arrived); this is the operator's way out:
//
//   - confirm: they checked and it arrived — ambiguous → confirmed (the text is
//     dropped, as for every confirmed intent);
//   - resend: send it ONCE more, re-authorising the ingress first exactly as a
//     delivery does, under the same txn id (a far side that dedups on it sees one
//     message): sent → confirmed; a permanent refusal → failed; anything else stays
//     ambiguous and the error is returned, so it is never silently retried.
func (s *DeliveryService) ResolveAmbiguous(ctx context.Context, key, action, reason string) error {
	if s.journal == nil {
		return domain.ErrDeliveryJournalOff
	}
	intents, err := s.journal.ListDeliveries(ctx, []domain.DeliveryIntentState{domain.DeliveryAmbiguous}, 0)
	if err != nil {
		return fmt.Errorf("delivery: list ambiguous intents: %w", err)
	}
	var in *domain.DeliveryIntent
	for i := range intents {
		if intents[i].Key == key {
			in = &intents[i]
			break
		}
	}
	if in == nil {
		all, lerr := s.journal.ListDeliveries(ctx, []domain.DeliveryIntentState{
			domain.DeliveryPrepared, domain.DeliveryDispatched, domain.DeliveryConfirmed, domain.DeliveryFailed}, 0)
		if lerr == nil {
			for _, x := range all {
				if x.Key == key {
					return fmt.Errorf("%w: %s is %s", domain.ErrDeliveryIntentNotAmbiguous, key, x.State)
				}
			}
		}
		return fmt.Errorf("%w: %s", domain.ErrDeliveryIntentNotFound, key)
	}
	note := "operator: " + reason
	switch action {
	case domain.DeliveryResolveConfirm:
		return s.journal.AdvanceDelivery(ctx, key, domain.DeliveryAmbiguous, domain.DeliveryConfirmed, note)
	case domain.DeliveryResolveResend:
		if s.transport == nil {
			return errors.New("delivery: no ingress transport configured")
		}
		if in.Text == "" {
			return fmt.Errorf("delivery: intent %s keeps no text to resend", key)
		}
		if err := s.authorise(ctx, in.Address); err != nil {
			return err
		}
		sendErr := s.transport.Deliver(ctx, domain.IngressDelivery{
			ConversationID: in.ConversationID,
			Address:        in.Address,
			Text:           in.Text,
			TxnID:          in.TxnID,
		})
		sctx := context.WithoutCancel(ctx)
		switch {
		case sendErr == nil:
			s.logger.Info("ADR-0090: ambiguous delivery resent by an operator and confirmed",
				"conversation", in.ConversationID, "key", key)
			return s.journal.AdvanceDelivery(sctx, key, domain.DeliveryAmbiguous, domain.DeliveryConfirmed, note+" (resent)")
		case errors.Is(sendErr, domain.ErrDeliveryPermanent):
			if err := s.journal.AdvanceDelivery(sctx, key, domain.DeliveryAmbiguous, domain.DeliveryFailed, sendErr.Error()); err != nil {
				s.logger.Error("ADR-0090: a resend's permanent failure could not be recorded", "key", key, "err", err)
			}
			return sendErr
		default:
			// Still unknown: it stays ambiguous, and the person sees why.
			return fmt.Errorf("%w: %v", domain.ErrDeliveryAmbiguous, sendErr)
		}
	default:
		return fmt.Errorf("delivery: action must be %q or %q, got %q", domain.DeliveryResolveConfirm, domain.DeliveryResolveResend, action)
	}
}

// settle moves a dispatched intent to its outcome state, logging (not returning)
// a journal failure: the delivery outcome is what the caller needs.
func (s *DeliveryService) settle(ctx context.Context, key string, to domain.DeliveryIntentState, cause error) {
	if err := s.journal.AdvanceDelivery(ctx, key, domain.DeliveryDispatched, to, cause.Error()); err != nil {
		s.logger.Error("ADR-0090: delivery outcome could not be recorded",
			"key", key, "to", string(to), "cause", cause, "err", err)
	}
}

// Start runs the redriver until Stop (ADR-0090 D8.1). No journal ⇒ no-op.
func (s *DeliveryService) Start(ctx context.Context) {
	if s.journal == nil {
		return
	}
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if s.cancel != nil {
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	interval := s.RedriveInterval
	if interval <= 0 {
		interval = DefaultRedriveInterval
	}
	done := s.done
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				s.Redrive(rctx)
			}
		}
	}()
}

// Stop ends the redriver and waits for it.
func (s *DeliveryService) Stop() {
	s.stopMu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.stopMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Redrive runs one recovery pass (ADR-0090 D8.1):
//
//   - a prepared intent past the grace period and its backoff is re-sent after
//     re-authorising its ingress — a crash before dispatch, or a transient failure;
//   - a prepared intent that has used MaxAttempts dispatches is failed;
//   - a dispatched intent past the grace period (the process died mid-send) is
//     marked ambiguous;
//   - settled and ambiguous intents older than DeliveryRetention are pruned.
func (s *DeliveryService) Redrive(ctx context.Context) {
	if s.journal == nil {
		return
	}
	now := s.now().UTC()
	stuck, err := s.journal.ListDeliveries(ctx,
		[]domain.DeliveryIntentState{domain.DeliveryPrepared, domain.DeliveryDispatched}, redriveBatch)
	if err != nil {
		s.logger.Warn("ADR-0090: delivery redrive could not list intents", "err", err)
		return
	}
	for _, in := range stuck {
		if ctx.Err() != nil {
			return
		}
		if now.Sub(in.UpdatedAt) < s.RedriveGrace {
			continue
		}
		switch in.State {
		case domain.DeliveryDispatched:
			if err := s.journal.AdvanceDelivery(ctx, in.Key, domain.DeliveryDispatched, domain.DeliveryAmbiguous,
				"process stopped during the send; outcome unknown"); err == nil {
				s.logger.Error("ADR-0090: delivery interrupted mid-send; recorded as ambiguous and NOT resent",
					"conversation", in.ConversationID, "key", in.Key)
			}
		case domain.DeliveryPrepared:
			s.redriveOne(ctx, in, now)
		}
	}
	if s.DeliveryRetention > 0 {
		if n, _, perr := s.journal.PruneDeliveries(ctx, now.Add(-s.DeliveryRetention), redriveBatch); perr != nil {
			s.logger.Warn("ADR-0090: delivery journal prune failed", "err", perr)
		} else if n > 0 {
			s.logger.Info("ADR-0090: pruned settled delivery intents", "removed", n)
		}
	}
}

func (s *DeliveryService) redriveOne(ctx context.Context, in domain.DeliveryIntent, now time.Time) {
	maxAttempts := s.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	if in.Attempts >= maxAttempts {
		if err := s.journal.AdvanceDelivery(ctx, in.Key, domain.DeliveryPrepared, domain.DeliveryFailed,
			fmt.Sprintf("gave up after %d attempts: %s", in.Attempts, in.Err)); err == nil {
			s.logger.Error("ADR-0090: delivery retries exhausted; recorded as failed",
				"conversation", in.ConversationID, "key", in.Key, "attempts", in.Attempts, "last_err", in.Err)
		}
		return
	}
	if now.Sub(in.UpdatedAt) < redriveBackoff(in.Attempts) {
		return
	}
	// D8: revocation must stop messages already addressed through an ingress.
	if err := s.authorise(ctx, in.Address); err != nil {
		if aerr := s.journal.AdvanceDelivery(ctx, in.Key, domain.DeliveryPrepared, domain.DeliveryFailed, err.Error()); aerr == nil {
			s.logger.Warn("ADR-0090: queued delivery dropped because its ingress was revoked; recorded as failed",
				"conversation", in.ConversationID, "key", in.Key, "err", err)
		}
		return
	}
	if in.Text == "" {
		// A malformed intent must not loop; failing it keeps it visible.
		_ = s.journal.AdvanceDelivery(ctx, in.Key, domain.DeliveryPrepared, domain.DeliveryFailed,
			"intent has no text to resend")
		return
	}
	// The outcome is recorded on the intent by dispatch.
	_ = s.dispatch(ctx, in, domain.IngressDelivery{
		ConversationID: in.ConversationID,
		Address:        in.Address,
		Text:           in.Text,
		TxnID:          in.TxnID,
	})
}

// redriveBackoff is the wait after the attempts made before the next redrive:
// exponential from 10 s, capped at maxRedriveBackoff.
func redriveBackoff(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	if attempts > 16 {
		return maxRedriveBackoff
	}
	b := time.Duration(1<<uint(attempts-1)) * 10 * time.Second
	if b > maxRedriveBackoff {
		return maxRedriveBackoff
	}
	return b
}

// authorise re-checks that the bound ingress is still registered and still
// permitted to speak for this recipient.
func (s *DeliveryService) authorise(ctx context.Context, addr domain.DeliveryAddress) error {
	if s.ingresses == nil {
		// No registry: nothing was ever registered, so nothing can be revoked. This
		// is the OSS shape, where an address could only have been bound by a caller
		// that already had the standing to do it.
		return nil
	}
	reg, ok := s.ingresses.ResolveIngress(ctx, domain.AgentPrincipal(addr.IngressAgentID))
	if !ok || reg.IsZero() {
		return fmt.Errorf("%w: %s", domain.ErrIngressRevoked, addr.IngressAgentID)
	}
	if !reg.MaySpeakFor(addr.ExternalID) {
		// The namespace was narrowed after this address was bound.
		return fmt.Errorf("%w: %s may no longer speak for %s",
			domain.ErrIngressRevoked, addr.IngressAgentID, addr.ExternalID)
	}
	return nil
}

// progressEnabled reports whether the address's ingress opted into progress
// snapshots (domain.IngressRegistration.Progress). No registry ⇒ true.
func (s *DeliveryService) progressEnabled(ctx context.Context, addr domain.DeliveryAddress) bool {
	if s.ingresses == nil {
		return true
	}
	reg, ok := s.ingresses.ResolveIngress(ctx, domain.AgentPrincipal(addr.IngressAgentID))
	return ok && reg.Progress
}

// classify reports a best-effort (journal-less) delivery failure. Without a
// journal the log is the only trace of a permanent failure; any failure is
// returned for the caller to act on.
//
// Only the ingress knows whether a failure is permanent, so it says so with
// domain.PermanentDelivery.
func (s *DeliveryService) classify(d domain.IngressDelivery, err error) error {
	if errors.Is(err, domain.ErrDeliveryPermanent) {
		s.logger.Warn("ADR-0090: permanently undeliverable, and no journal to record it",
			"conversation", d.ConversationID, "address", d.Address.String(), "reason", err)
	}
	return err
}

// deliveryKey namespaces the idempotency key by conversation, so two
// conversations cannot collide on a caller's message id.
func deliveryKey(conversationID, txnID string) string {
	return "delivery:" + conversationID + ":" + txnID
}
