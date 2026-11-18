package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
)

// TableDerivationJobs is the ENG-2122 derivation journal (migration 0058).
const TableDerivationJobs = "derivation_jobs"

var _ domain.DerivationJournal = (*PgVectorAdapter)(nil)

// derivationPayload is the JSONB payload column: what the in-memory job carried on its
// context. Field names are the stored contract; never rename one.
type derivationPayload struct {
	WriterID     string           `json:"writer_id,omitempty"`
	WriterKind   string           `json:"writer_kind,omitempty"`
	EvidenceID   string           `json:"evidence_id,omitempty"`
	IngestOrigin *time.Time       `json:"ingest_origin,omitempty"`
	Source       derivationSource `json:"source"`
}

type derivationSource struct {
	SourceType     string     `json:"source_type,omitempty"`
	Title          string     `json:"title,omitempty"`
	Author         string     `json:"author,omitempty"`
	ReferenceTime  *time.Time `json:"reference_time,omitempty"`
	ReferenceBasis string     `json:"reference_basis,omitempty"`
	TimePrecision  string     `json:"time_precision,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func derivationPayloadOf(j domain.DerivationJob) ([]byte, error) {
	p := derivationPayload{
		WriterID: j.Writer.ID, WriterKind: string(j.Writer.Kind), EvidenceID: string(j.EvidenceID),
		IngestOrigin: timePtr(j.IngestOrigin),
		Source: derivationSource{
			SourceType: j.Source.SourceType, Title: j.Source.Title, Author: j.Source.Author,
			ReferenceTime: timePtr(j.Source.ReferenceTime), ReferenceBasis: string(j.Source.ReferenceBasis),
			TimePrecision: string(j.Source.TimePrecision),
		},
	}
	return json.Marshal(p)
}

func (p derivationPayload) apply(j *domain.DerivationJob) {
	j.Writer = domain.PrincipalRef{ID: p.WriterID, Kind: domain.PrincipalKind(p.WriterKind)}
	j.EvidenceID = domain.EvidenceID(p.EvidenceID)
	if p.IngestOrigin != nil {
		j.IngestOrigin = p.IngestOrigin.UTC()
	}
	j.Source = domain.SourceMeta{
		SourceType: p.Source.SourceType, Title: p.Source.Title, Author: p.Source.Author,
		ReferenceBasis: domain.ReferenceBasis(p.Source.ReferenceBasis),
		TimePrecision:  domain.TimePrecision(p.Source.TimePrecision),
	}
	if p.Source.ReferenceTime != nil {
		j.Source.ReferenceTime = p.Source.ReferenceTime.UTC()
	}
}

// RecordDerivations implements domain.DerivationJournal: one upsert per job, in one
// transaction. A re-ingest of the document replaces its row and resets its attempts.
func (p *PgVectorAdapter) RecordDerivations(ctx context.Context, jobs []domain.DerivationJob, lease time.Duration) error {
	if len(jobs) == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return mapError("RecordDerivations", err)
	}
	defer tx.Rollback(ctx)
	q := `INSERT INTO ` + TableDerivationJobs + ` (kind, document_id, content_digest, chunk_ids, payload, claimed_at, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, now(), now() + make_interval(secs => $6))
		ON CONFLICT (kind, document_id) DO UPDATE SET
			content_digest = EXCLUDED.content_digest, chunk_ids = EXCLUDED.chunk_ids, payload = EXCLUDED.payload,
			attempts = 0, last_error = '', dead_at = NULL, created_at = now(),
			claimed_at = EXCLUDED.claimed_at, next_attempt_at = EXCLUDED.next_attempt_at`
	for _, j := range jobs {
		if j.DocumentID == "" {
			return fmt.Errorf("RecordDerivations: %s job without a document id", j.Kind)
		}
		payload, err := derivationPayloadOf(j)
		if err != nil {
			return fmt.Errorf("RecordDerivations: payload: %w", err)
		}
		ids := j.ChunkIDs
		if ids == nil {
			ids = []string{}
		}
		if _, err := tx.Exec(ctx, q, string(j.Kind), j.DocumentID, j.ContentDigest, ids, payload, lease.Seconds()); err != nil {
			return mapError("RecordDerivations", err)
		}
	}
	return mapError("RecordDerivations", tx.Commit(ctx))
}

// CompleteDerivation implements domain.DerivationJournal.
func (p *PgVectorAdapter) CompleteDerivation(ctx context.Context, k domain.DerivationKey) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM `+TableDerivationJobs+` WHERE kind = $1 AND document_id = $2 AND content_digest = $3`,
		string(k.Kind), k.DocumentID, k.ContentDigest)
	return mapError("CompleteDerivation", err)
}

// RescheduleDerivation implements domain.DerivationJournal.
func (p *PgVectorAdapter) RescheduleDerivation(ctx context.Context, k domain.DerivationKey, retryAt time.Time, lastError string, countAttempt bool, maxAttempts int) (bool, error) {
	inc := 0
	if countAttempt {
		inc = 1
	}
	var dead bool
	err := p.pool.QueryRow(ctx, `UPDATE `+TableDerivationJobs+` SET
			attempts = attempts + $4,
			last_error = $5,
			next_attempt_at = $6,
			dead_at = CASE WHEN $7 > 0 AND attempts + $4 >= $7 THEN now() ELSE NULL END
		WHERE kind = $1 AND document_id = $2 AND content_digest = $3
		RETURNING dead_at IS NOT NULL`,
		string(k.Kind), k.DocumentID, k.ContentDigest, inc, truncateError(lastError), retryAt.UTC(), maxAttempts).Scan(&dead)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // completed or replaced meanwhile
	}
	return dead, mapError("RescheduleDerivation", err)
}

// truncateError bounds a stored error text.
func truncateError(s string) string {
	const max = 2000
	if len(s) > max {
		return s[:max]
	}
	return s
}

// ClaimDerivations implements domain.DerivationJournal.
func (p *PgVectorAdapter) ClaimDerivations(ctx context.Context, kind domain.DerivationKind, since time.Time, limit int, lease time.Duration) ([]domain.DerivationJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `UPDATE `+TableDerivationJobs+` j SET claimed_at = now(), next_attempt_at = now() + make_interval(secs => $4)
		FROM (
			SELECT kind, document_id FROM `+TableDerivationJobs+`
			WHERE kind = $1 AND dead_at IS NULL AND (claimed_at < $2 OR next_attempt_at <= now())
			ORDER BY next_attempt_at, created_at, document_id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		) due
		WHERE j.kind = due.kind AND j.document_id = due.document_id
		RETURNING j.kind, j.document_id, j.content_digest, j.chunk_ids, j.payload, j.attempts`,
		string(kind), since.UTC(), limit, lease.Seconds())
	if err != nil {
		return nil, mapError("ClaimDerivations", err)
	}
	defer rows.Close()
	var out []domain.DerivationJob
	for rows.Next() {
		var (
			j       domain.DerivationJob
			k       string
			payload []byte
		)
		if err := rows.Scan(&k, &j.DocumentID, &j.ContentDigest, &j.ChunkIDs, &payload, &j.Attempts); err != nil {
			return nil, mapError("ClaimDerivations", err)
		}
		j.Kind = domain.DerivationKind(k)
		var pl derivationPayload
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &pl); err != nil {
				return nil, fmt.Errorf("ClaimDerivations: payload of %s/%s: %w", k, j.DocumentID, err)
			}
		}
		pl.apply(&j)
		out = append(out, j)
	}
	return out, mapError("ClaimDerivations", rows.Err())
}

// DerivationBacklog implements domain.DerivationJournal.
func (p *PgVectorAdapter) DerivationBacklog(ctx context.Context) (map[domain.DerivationKind]domain.DerivationBacklog, error) {
	rows, err := p.pool.Query(ctx, `SELECT kind,
			count(*) FILTER (WHERE dead_at IS NULL),
			count(*) FILTER (WHERE dead_at IS NULL AND attempts > 0),
			count(*) FILTER (WHERE dead_at IS NOT NULL),
			min(created_at) FILTER (WHERE dead_at IS NULL)
		FROM `+TableDerivationJobs+` GROUP BY kind`)
	if err != nil {
		return nil, mapError("DerivationBacklog", err)
	}
	defer rows.Close()
	out := map[domain.DerivationKind]domain.DerivationBacklog{}
	for rows.Next() {
		var (
			k      string
			b      domain.DerivationBacklog
			oldest *time.Time
		)
		if err := rows.Scan(&k, &b.Pending, &b.Retrying, &b.Dead, &oldest); err != nil {
			return nil, mapError("DerivationBacklog", err)
		}
		if oldest != nil {
			b.Oldest = oldest.UTC()
		}
		out[domain.DerivationKind(k)] = b
	}
	return out, mapError("DerivationBacklog", rows.Err())
}
