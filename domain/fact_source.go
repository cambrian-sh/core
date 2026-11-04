package domain

import (
	"strings"
	"time"
)

// ADR-0136 Amendment A2.1: what differs per ingested source is DATA, taken from the
// source's own ingest metadata and handed to the fact extractor — never a branch in a
// prompt (decisions/ingestion-prompts-are-source-agnostic.md). The extractor renders it
// as the prompt's `Source:` block; nothing switches on any field.

// ReferenceBasis says what a source's reference time is (A2.1, A2.2 rule 5).
type ReferenceBasis string

const (
	// ReferenceBasisEvent: the caller stated when the content happened (an explicit
	// occurred_at or part times).
	ReferenceBasisEvent ReferenceBasis = "event"
	// ReferenceBasisAuthored: the time was read from the source itself (its own date —
	// source metadata or a declared inference rule), so it is when the text was written.
	ReferenceBasisAuthored ReferenceBasis = "authored"
	// ReferenceBasisIngested: nothing was known; the reference time is the ingest clock,
	// which says nothing about when the text was written.
	ReferenceBasisIngested ReferenceBasis = "ingested"
)

// DatesWriting reports whether the reference time is a time of writing, so a fact
// the text presents as holding "now" may start on it (A2.2 rule 5; owner answer
// A2-Q4: authored qualifies). An ingest clock never does.
func (b ReferenceBasis) DatesWriting() bool {
	return b == ReferenceBasisEvent || b == ReferenceBasisAuthored
}

// SourceMeta is one source's ingest metadata as the fact extractor sees it.
type SourceMeta struct {
	// SourceType is the free-text source label: the client's ExternalDocument.SourceKind
	// when it gave one (ADR-0136 A3.2), else the kernel's SourceType. Shown to the model
	// as context only; never an enum anything branches on.
	SourceType string
	// Title is the source's title (the client's, else the body's first line — both
	// already resolved onto ExternalDocument.Title by the ingest plane).
	Title string
	// Author is the source's author as the source names it (ExternalDocument.AuthorLabel),
	// else the stamped writer (ExternalDocument.Author), else the writing principal's id,
	// else "". It resolves first-person references no passage labels.
	Author string
	// ReferenceTime is the "now" relative dates resolve against; ReferenceBasis says
	// what it is. Zero time = unknown.
	ReferenceTime  time.Time
	ReferenceBasis ReferenceBasis
	// TimePrecision qualifies ReferenceTime ("" = unknown / second).
	TimePrecision TimePrecision
}

// ReferenceBasisOf maps the as-built event-time label onto a reference basis.
//
// The A2.1 table names three sources (EventTime → event, Timestamp → authored, ingest
// time → ingested); the as-built ExternalDocument carries ONE time (Timestamp, ADR-0134)
// qualified by TimeSource, so the basis is read from the label: a caller-stated time is
// an event time, a time read from the source itself is its authored time, and a
// defaulted or clamped time is the ingest clock. A zero time is always ingested.
func ReferenceBasisOf(src TimeSource, t time.Time) ReferenceBasis {
	if t.IsZero() {
		return ReferenceBasisIngested
	}
	switch src {
	case TimeSourceExplicit:
		return ReferenceBasisEvent
	case TimeSourceSourceMetadata, TimeSourceInferred:
		return ReferenceBasisAuthored
	}
	// ingest_default, clamped_future (the stored time IS the ingest clock) and the empty
	// label (reads as ingest_default).
	return ReferenceBasisIngested
}

// SourceMetaOf builds a document's SourceMeta at ingest. ingestedAt stands in for an
// unstated time; principal is the writing principal's id, the author fallback.
func SourceMetaOf(doc ExternalDocument, ingestedAt time.Time, principal string) SourceMeta {
	m := SourceMeta{
		SourceType:    firstNonBlank(doc.SourceKind, doc.SourceType),
		Title:         strings.TrimSpace(doc.Title),
		Author:        firstNonBlank(doc.AuthorLabel, doc.Author),
		TimePrecision: doc.TimePrecision,
	}
	if m.Author == "" {
		m.Author = strings.TrimSpace(principal)
	}
	m.ReferenceTime = doc.Timestamp.UTC()
	m.ReferenceBasis = ReferenceBasisOf(doc.TimeSource, doc.Timestamp)
	if doc.Timestamp.IsZero() {
		m.ReferenceTime = ingestedAt.UTC()
		m.TimePrecision = ""
	}
	return m
}

// firstNonBlank returns the first value that is not blank, trimmed (A3.2 precedence:
// the client's descriptive value, else the kernel's).
func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
