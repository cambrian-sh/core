package domain

import (
	"errors"
	"unicode"
	"unicode/utf8"
)

// ErrNoUsableText refuses an ingest whose content yields no text the memory can
// use: an empty or whitespace-only body, a file the parser could not read, an image
// with no recognised text (ENG-2110, owner decision 2026-11-18: unusable content is
// not a success). It is returned wrapped as
// "no usable text could be extracted from <name>: <why>", before anything of the
// document is written — no evidence, no document row, no chunks, no derivation
// journal. Callers match it with errors.Is and answer the caller's error
// (INVALID_ARGUMENT / 400), never Internal.
var ErrNoUsableText = errors.New("no usable text could be extracted")

// HasUsableText reports whether s carries at least one rune a reader could use: a
// printable, non-space rune other than U+FFFD (the replacement character a decoder
// leaves for bytes it could not read, so a body made of undecodable bytes is not
// "text").
func HasUsableText(s string) bool {
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsSpace(r) {
			continue
		}
		if unicode.IsPrint(r) || unicode.IsGraphic(r) {
			return true
		}
	}
	return false
}

// StorableText returns s with every byte Postgres text refuses replaced by a space:
// NUL (0x00, "invalid byte sequence for encoding UTF8: 0x00", and the jsonb
// "unsupported Unicode escape sequence" 22P05) and each byte of an invalid UTF-8
// sequence. The replacement is byte for byte, so offsets into the string — the
// DocumentPart offsets of a multi-part body — stay correct; deleting the bytes would
// shift every later part. Returns s unchanged (no allocation) when it is clean.
func StorableText(s string) string {
	clean := true
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == 0 || (r == utf8.RuneError && n == 1) {
			clean = false
			break
		}
		i += n
	}
	if clean {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == 0:
			out = append(out, ' ')
		case r == utf8.RuneError && n == 1:
			out = append(out, ' ')
		default:
			out = append(out, s[i:i+n]...)
		}
		i += n
	}
	return string(out)
}

// NormalizeForStorage makes every text field of the document storable (StorableText):
// a NUL byte in a pushed body used to fail deep in Postgres with a raw driver error,
// after the evidence row was already written (ENG-2110). Applied once at the head of
// the ingestion pipeline, before the document's identity digest, so the evidence, the
// document row and the chunks all hold the same bytes. Data (binary uploads, stored
// as bytea) is untouched.
func (d *ExternalDocument) NormalizeForStorage() {
	d.SourceURI = StorableText(d.SourceURI)
	d.SourceType = StorableText(d.SourceType)
	d.Title = StorableText(d.Title)
	d.Body = StorableText(d.Body)
	d.Author = StorableText(d.Author)
	d.SourceKind = StorableText(d.SourceKind)
	d.AuthorLabel = StorableText(d.AuthorLabel)
	d.ThreadID = StorableText(d.ThreadID)
	d.UploadName = StorableText(d.UploadName)
	// The slices may be shared with the caller's request: copy before the first
	// change rather than rewrite the caller's backing array.
	for i, t := range d.Tags {
		if c := StorableText(t); c != t {
			d.Tags = append([]string(nil), d.Tags...)
			d.Tags[i] = c
		}
	}
	for i, p := range d.Parts {
		if c := StorableText(p.Speaker); c != p.Speaker {
			d.Parts = append([]DocumentPart(nil), d.Parts...)
			d.Parts[i].Speaker = c
		}
	}
	for i, a := range d.Attachments {
		n, m, b := StorableText(a.Name), StorableText(a.MIMEType), StorableText(a.Body)
		if n != a.Name || m != a.MIMEType || b != a.Body {
			d.Attachments = append([]Attachment(nil), d.Attachments...)
			d.Attachments[i] = Attachment{Name: n, MIMEType: m, Body: b}
		}
	}
}

// IngestDisplayName names a document in a refusal the caller reads: the uploaded
// file name, else the title, else the source URI.
func (d ExternalDocument) IngestDisplayName() string {
	switch {
	case d.UploadName != "":
		return d.UploadName
	case d.Title != "":
		return d.Title
	case d.SourceURI != "":
		return d.SourceURI
	}
	return "the document"
}
