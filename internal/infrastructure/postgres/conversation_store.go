package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgConversationStore is the Postgres-backed domain.ConversationStore (ADR-0084 D1).
//
// Unlike the older operator_audit store, this one does NOT create its own tables: schema is
// owned by the versioned migration runner (PLAT-02 / ADR-0064), so the DDL lives in exactly
// one place — migrations/0002_conversations.sql — and cannot drift from a second copy
// embedded in Go. The constructor verifies the table is present and says what to run if not.
type PgConversationStore struct {
	pool *pgxpool.Pool
}

var _ domain.ConversationStore = (*PgConversationStore)(nil)

const msgCols = `id, conversation_id, seq, role, content, COALESCE(client_id, ''), created_at, source_refs, redacted_at, own_source_refs`

// NewPgConversationStore returns the store, failing with an actionable error when the
// schema has not been migrated (the only realistic cause is storage.auto_migrate=false).
func NewPgConversationStore(ctx context.Context, pool *pgxpool.Pool) (*PgConversationStore, error) {
	// Unqualified, like every query below: the guard must probe the relation the store will
	// actually use (search_path), not public's — a scratch-schema test once had to drop the
	// migrated table to make this check fail (T-20261114-postgres-adapter-tests-drop-migrated-tables).
	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('conversations') IS NOT NULL`).Scan(&present); err != nil {
		return nil, fmt.Errorf("check conversations schema: %w", err)
	}
	if !present {
		return nil, errors.New("conversations table is missing: run `cambrian migrate up` " +
			"(or leave storage.auto_migrate at its default of true). Schema is owned by " +
			"migration 0002_conversations.sql — ADR-0064")
	}
	return &PgConversationStore{pool: pool}, nil
}

// CreateConversation persists a new conversation.
func (s *PgConversationStore) CreateConversation(ctx context.Context, c domain.Conversation) error {
	if err := c.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO conversations (id, owner_id, title, status, profile, policy, created_at, updated_at,
		                           delivery_ingress, delivery_external)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		c.ID, c.OwnerID, c.Title, string(c.Status), string(c.Profile), c.Policy, c.CreatedAt, c.UpdatedAt,
		c.Delivery.IngressAgentID, c.Delivery.ExternalID)
	if err != nil {
		return fmt.Errorf("create conversation: %w", err)
	}
	return nil
}

// GetConversation returns a conversation or domain.ErrConversationNotFound.
func (s *PgConversationStore) GetConversation(ctx context.Context, id string) (*domain.Conversation, error) {
	var (
		c      domain.Conversation
		status string
		prof   string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, owner_id, title, status, profile, policy, created_at, updated_at,
		       delivery_ingress, delivery_external
		FROM conversations WHERE id = $1`, id).
		Scan(&c.ID, &c.OwnerID, &c.Title, &status, &prof, &c.Policy, &c.CreatedAt, &c.UpdatedAt,
			&c.Delivery.IngressAgentID, &c.Delivery.ExternalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation: %w", err)
	}
	c.Status = domain.ConversationStatus(status)
	c.Profile = domain.ConversationProfile(prof)
	return &c, nil
}

// ListConversations returns an owner's conversations, most recently updated first. A blank
// ownerID lists across owners — administrative use only, never an end-user path.
func (s *PgConversationStore) ListConversations(ctx context.Context, ownerID string, limit int) ([]domain.Conversation, error) {
	q := `SELECT id, owner_id, title, status, profile, policy, created_at, updated_at
	      FROM conversations`
	args := []any{}
	if ownerID != "" {
		q += ` WHERE owner_id = $1`
		args = append(args, ownerID)
	}
	q += ` ORDER BY updated_at DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	var out []domain.Conversation
	for rows.Next() {
		var (
			c      domain.Conversation
			status string
			prof   string
		)
		if err := rows.Scan(&c.ID, &c.OwnerID, &c.Title, &status, &prof, &c.Policy, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		c.Status = domain.ConversationStatus(status)
		c.Profile = domain.ConversationProfile(prof)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetConversationStatus opens or closes a conversation.
func (s *PgConversationStore) SetConversationStatus(ctx context.Context, id string, status domain.ConversationStatus) error {
	if status != domain.ConversationOpen && status != domain.ConversationClosed {
		return errors.New("conversation status must be open or closed")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations SET status = $2, updated_at = $3 WHERE id = $1`,
		id, string(status), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("set conversation status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrConversationNotFound
	}
	return nil
}

// AppendMessage assigns the next Seq and stores the message.
//
// Seq assignment happens inside the transaction as an UPDATE ... RETURNING on the
// conversation row, which takes the row lock: two concurrent turns serialize on it and can
// never claim the same position. Doing this as SELECT MAX(seq)+1 would race.
//
// The same statement enforces the open/closed gate — status is part of the WHERE — so a
// closed conversation cannot be appended to even under a concurrent close.
func (s *PgConversationStore) AppendMessage(ctx context.Context, m domain.Message) (domain.Message, error) {
	if err := m.Validate(); err != nil {
		return domain.Message{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Message{}, fmt.Errorf("begin append: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotent replay: a retried turn returns what was already stored.
	if m.ClientID != "" {
		existing, err := scanMessage(tx.QueryRow(ctx,
			`SELECT `+msgCols+` FROM conversation_messages
			 WHERE conversation_id = $1 AND client_id = $2`, m.ConversationID, m.ClientID))
		switch {
		case err == nil:
			return existing, tx.Commit(ctx)
		case !errors.Is(err, pgx.ErrNoRows):
			return domain.Message{}, fmt.Errorf("append idempotency check: %w", err)
		}
	}

	now := time.Now().UTC()
	var seq int64
	err = tx.QueryRow(ctx, `
		UPDATE conversations SET next_seq = next_seq + 1, updated_at = $2
		WHERE id = $1 AND status = $3
		RETURNING next_seq - 1`,
		m.ConversationID, now, string(domain.ConversationOpen)).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		// The UPDATE matched nothing: either no such conversation, or it is closed.
		// Distinguish them so a caller can return the right status to a client.
		var status string
		e := tx.QueryRow(ctx, `SELECT status FROM conversations WHERE id = $1`, m.ConversationID).Scan(&status)
		if errors.Is(e, pgx.ErrNoRows) {
			return domain.Message{}, domain.ErrConversationNotFound
		}
		if e != nil {
			return domain.Message{}, fmt.Errorf("append: resolve conversation state: %w", e)
		}
		return domain.Message{}, domain.ErrConversationClosed
	}
	if err != nil {
		return domain.Message{}, fmt.Errorf("append: claim seq: %w", err)
	}

	m.Seq = seq
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	var clientID *string
	if m.ClientID != "" {
		clientID = &m.ClientID
	}
	// ENG-2109: an agent reply carries what it rests on, so an erase of any of those
	// records reaches it (PgErasureStore.redactReplies). Only agent replies: the
	// redaction never touches the person's own words or an interruption marker.
	refs, own := []string{}, []string{}
	if m.Role == domain.MessageRoleAgent {
		// ENG-2165: the own refs are a subset of the union the erase redacts by.
		own = domain.SortedUniqueIDs(m.OwnSourceRefs)
		refs = domain.SortedUniqueIDs(append(append([]string(nil), m.SourceRefs...), own...))
	}
	if refs == nil {
		refs = []string{} // '[]', never JSON null
	}
	if own == nil {
		own = []string{}
	}
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return domain.Message{}, fmt.Errorf("append: source refs: %w", err)
	}
	ownJSON, err := json.Marshal(own)
	if err != nil {
		return domain.Message{}, fmt.Errorf("append: own source refs: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO conversation_messages (id, conversation_id, seq, role, content, client_id, created_at, source_refs, own_source_refs)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb)`,
		m.ID, m.ConversationID, m.Seq, string(m.Role), m.Content, clientID, m.CreatedAt, string(refsJSON), string(ownJSON)); err != nil {
		return domain.Message{}, fmt.Errorf("append: insert message: %w", err)
	}
	if len(refs) == 0 {
		refs = nil
	}
	if len(own) == 0 {
		own = nil
	}
	m.SourceRefs, m.OwnSourceRefs = refs, own
	return m, tx.Commit(ctx)
}

// ListMessages returns messages with Seq strictly greater than afterSeq, in order.
func (s *PgConversationStore) ListMessages(ctx context.Context, conversationID string, afterSeq int64, limit int) ([]domain.Message, error) {
	q := `SELECT ` + msgCols + ` FROM conversation_messages
	      WHERE conversation_id = $1 AND seq > $2 ORDER BY seq`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := s.pool.Query(ctx, q, conversationID, afterSeq)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var out []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListUnansweredTurns returns the user messages created before `before` that are the
// latest message of an open conversation (ENG-2124). The latest message is the one at
// next_seq-1 (AppendMessage claims seq = next_seq, then increments), so this is a join
// on the (conversation_id, seq) unique index, not a scan for a max per conversation.
func (s *PgConversationStore) ListUnansweredTurns(ctx context.Context, before time.Time, limit int) ([]domain.Message, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.conversation_id, m.seq, m.role, m.content, COALESCE(m.client_id, ''), m.created_at,
		       m.source_refs, m.redacted_at, m.own_source_refs
		  FROM conversations c
		  JOIN conversation_messages m ON m.conversation_id = c.id AND m.seq = c.next_seq - 1
		 WHERE c.status = $1 AND m.role = $2 AND m.created_at < $3
		 ORDER BY m.created_at
		 LIMIT $4`,
		string(domain.ConversationOpen), string(domain.MessageRoleUser), before.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list unanswered turns: %w", err)
	}
	defer rows.Close()
	var out []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan unanswered turn: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// scanMessage maps one row of msgCols onto a domain.Message.
func scanMessage(row pgx.Row) (domain.Message, error) {
	var (
		m    domain.Message
		role string
		refs []byte
		own  []byte
	)
	if err := row.Scan(&m.ID, &m.ConversationID, &m.Seq, &role, &m.Content, &m.ClientID, &m.CreatedAt,
		&refs, &m.RedactedAt, &own); err != nil {
		return domain.Message{}, err
	}
	m.Role = domain.MessageRole(role)
	if len(refs) > 0 {
		// A JSONB array the store wrote (sourceRefsJSON); erase reach, never a read gate.
		if err := json.Unmarshal(refs, &m.SourceRefs); err != nil {
			return domain.Message{}, fmt.Errorf("message %s: source_refs: %w", m.ID, err)
		}
		if len(m.SourceRefs) == 0 {
			m.SourceRefs = nil
		}
	}
	// ENG-2165: own_source_refs is NULL on a row written before 0063 — every ref of
	// such a reply counts as its own (the broad rule it was written under).
	if own == nil {
		m.OwnSourceRefs = m.SourceRefs
	} else if err := json.Unmarshal(own, &m.OwnSourceRefs); err != nil {
		return domain.Message{}, fmt.Errorf("message %s: own_source_refs: %w", m.ID, err)
	} else if len(m.OwnSourceRefs) == 0 {
		m.OwnSourceRefs = nil
	}
	return m, nil
}

// BindDelivery records where replies to this conversation go (ADR-0090).
//
// Write-once, and enforced in the UPDATE's WHERE clause rather than by reading
// first and writing after: a check-then-set would let two inbound messages racing
// on first contact each see "unbound" and the second silently redirect the first.
// Here the second UPDATE simply matches no rows.
func (s *PgConversationStore) BindDelivery(ctx context.Context, conversationID string, addr domain.DeliveryAddress) error {
	if addr.IsZero() {
		return domain.ErrDeliveryAddressInvalid
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE conversations
		   SET delivery_ingress = $2, delivery_external = $3, updated_at = now()
		 WHERE id = $1 AND delivery_ingress = ''`,
		conversationID, addr.IngressAgentID, addr.ExternalID)
	if err != nil {
		return fmt.Errorf("bind delivery: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// No row moved: either the conversation is gone, or it was already bound. Those
	// are different problems for the caller, so tell them apart rather than
	// returning one vague error.
	var existing string
	err = s.pool.QueryRow(ctx, `SELECT delivery_ingress FROM conversations WHERE id = $1`, conversationID).Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrConversationNotFound
	}
	if err != nil {
		return fmt.Errorf("bind delivery: %w", err)
	}
	return domain.ErrDeliveryAlreadyBound
}

// FindByDelivery returns the conversation bound to addr, or ErrConversationNotFound.
//
// An unbound address matches nothing by construction: the index is partial on
// delivery_ingress <> ”, and a zero address is refused before the query, so an
// empty external id can never collide with the empty defaults of unbound rows.
func (s *PgConversationStore) FindByDelivery(ctx context.Context, addr domain.DeliveryAddress) (*domain.Conversation, error) {
	if addr.IsZero() {
		return nil, domain.ErrConversationNotFound
	}
	var (
		c            domain.Conversation
		status, prof string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, owner_id, title, status, profile, policy, created_at, updated_at,
		       delivery_ingress, delivery_external
		FROM conversations
		WHERE delivery_ingress = $1 AND delivery_external = $2
		ORDER BY created_at DESC
		LIMIT 1`, addr.IngressAgentID, addr.ExternalID).
		Scan(&c.ID, &c.OwnerID, &c.Title, &status, &prof, &c.Policy, &c.CreatedAt, &c.UpdatedAt,
			&c.Delivery.IngressAgentID, &c.Delivery.ExternalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find conversation by delivery: %w", err)
	}
	c.Status = domain.ConversationStatus(status)
	c.Profile = domain.ConversationProfile(prof)
	return &c, nil
}

// ListIngressTraffic reports who has come through an entry point (domain.IngressTrafficLister).
//
// One aggregate per conversation rather than a row per message: the console is
// asking "who came through", and a message-level list would bury one persistent
// stranger under a hundred rows from a colleague.
//
// `answered` compares the last inbound message against the last reply. A turn
// that failed silently — the agent produced nothing, or delivery never happened —
// leaves the person's message as the final word, and that is exactly the outcome
// worth seeing.
func (s *PgConversationStore) ListIngressTraffic(ctx context.Context, ingressAgentID string, limit int) ([]domain.InboundContact, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `
		SELECT c.id, c.delivery_ingress, c.delivery_external,
		       MIN(m.created_at) FILTER (WHERE m.role = 'user')  AS first_seen,
		       MAX(m.created_at) FILTER (WHERE m.role = 'user')  AS last_seen,
		       COUNT(*)          FILTER (WHERE m.role = 'user')  AS msg_count,
		       (ARRAY_AGG(m.content ORDER BY m.seq DESC) FILTER (WHERE m.role = 'user'))[1] AS last_text,
		       COALESCE(MAX(m.seq) FILTER (WHERE m.role <> 'user'), -1) >
		       COALESCE(MAX(m.seq) FILTER (WHERE m.role  = 'user'), -1) AS answered
		  FROM conversations c
		  JOIN conversation_messages m ON m.conversation_id = c.id
		 WHERE c.delivery_ingress <> ''`
	args := []any{}
	if ingressAgentID != "" {
		q += ` AND c.delivery_ingress = $1`
		args = append(args, ingressAgentID)
	}
	q += `
		 GROUP BY c.id, c.delivery_ingress, c.delivery_external
		 HAVING COUNT(*) FILTER (WHERE m.role = 'user') > 0
		 ORDER BY last_seen DESC
		 LIMIT ` + fmt.Sprint(limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list ingress traffic: %w", err)
	}
	defer rows.Close()

	var out []domain.InboundContact
	for rows.Next() {
		var (
			c                   domain.InboundContact
			firstSeen, lastSeen *time.Time
			lastText            *string
		)
		if err := rows.Scan(&c.ConversationID, &c.IngressAgentID, &c.ExternalID,
			&firstSeen, &lastSeen, &c.MessageCount, &lastText, &c.Answered); err != nil {
			return nil, fmt.Errorf("scan ingress traffic: %w", err)
		}
		if firstSeen != nil {
			c.FirstSeen = *firstSeen
		}
		if lastSeen != nil {
			c.LastSeen = *lastSeen
		}
		if lastText != nil {
			c.LastText = *lastText
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
