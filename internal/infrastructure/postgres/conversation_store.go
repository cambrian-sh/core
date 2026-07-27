package postgres

import (
	"context"
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

const msgCols = `id, conversation_id, seq, role, content, COALESCE(client_id, ''), created_at`

// NewPgConversationStore returns the store, failing with an actionable error when the
// schema has not been migrated (the only realistic cause is storage.auto_migrate=false).
func NewPgConversationStore(ctx context.Context, pool *pgxpool.Pool) (*PgConversationStore, error) {
	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.conversations') IS NOT NULL`).Scan(&present); err != nil {
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
	if _, err := tx.Exec(ctx, `
		INSERT INTO conversation_messages (id, conversation_id, seq, role, content, client_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		m.ID, m.ConversationID, m.Seq, string(m.Role), m.Content, clientID, m.CreatedAt); err != nil {
		return domain.Message{}, fmt.Errorf("append: insert message: %w", err)
	}
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
		var (
			m    domain.Message
			role string
		)
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Seq, &role, &m.Content, &m.ClientID, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Role = domain.MessageRole(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// scanMessage maps one row of msgCols onto a domain.Message.
func scanMessage(row pgx.Row) (domain.Message, error) {
	var (
		m    domain.Message
		role string
	)
	if err := row.Scan(&m.ID, &m.ConversationID, &m.Seq, &role, &m.Content, &m.ClientID, &m.CreatedAt); err != nil {
		return domain.Message{}, err
	}
	m.Role = domain.MessageRole(role)
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
// delivery_ingress <> '', and a zero address is refused before the query, so an
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
