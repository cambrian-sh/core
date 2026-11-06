package stepoutput

import (
	"strings"
	"unicode/utf8"
)

// Extractor pulls the string value of one top-level key out of a JSON object that
// arrives in pieces — a streamed tool call's arguments — and hands back the decoded
// text as soon as it is known (ADR-0139 amendment 2026-11-06: the final-answer
// argument is forwarded live).
//
// It is a byte-level state machine, not a parser: it never buffers the document,
// only the key being read and an incomplete UTF-8 tail. It handles every JSON string
// escape, \u escapes split across pieces, surrogate pairs (an unpaired surrogate
// becomes U+FFFD, as encoding/json does), multibyte UTF-8 split across pieces, and
// nested objects/arrays (a matching key below the top level is not the argument).
//
// When the key's value is not a string (a number, an object, null) the extractor
// produces nothing for it: the live stream simply has no delta, and the complete
// response still carries the call. Feed is not safe for concurrent use.
type Extractor struct {
	key string

	depth    int
	topObj   bool // the top-level container is an object
	inStr    bool
	role     strRole
	esc      bool
	uLeft    int  // hex digits still to read in a \u escape; 0 = none
	uVal     rune // the \u value read so far
	hi       rune // a pending high surrogate (0 = none)
	expKey   bool // at depth 1: the next string is a key
	expValue bool // at depth 1: after ':' — the next token is a value
	keyBuf   strings.Builder
	lastKey  string
	done     bool // the target value was read (or was not a string)

	out []byte // decoded value bytes not yet returned
}

type strRole int

const (
	roleSkip strRole = iota
	roleKey
	roleValue
)

// NewExtractor extracts the top-level key's string value.
func NewExtractor(key string) *Extractor { return &Extractor{key: key} }

// Done reports whether the target value has been read to its closing quote (or
// turned out not to be a string). Further input is ignored once done.
func (e *Extractor) Done() bool { return e.done }

// Feed consumes the next piece of JSON text and returns the newly decoded part of
// the target value: always valid UTF-8, possibly empty. A multibyte character or
// an escape split across pieces is returned once complete.
func (e *Extractor) Feed(piece string) string {
	for i := 0; i < len(piece); i++ {
		if e.done && !e.inStr {
			break
		}
		e.step(piece[i])
	}
	return e.drain()
}

func (e *Extractor) step(b byte) {
	if e.inStr {
		e.stringByte(b)
		return
	}
	switch b {
	case ' ', '\t', '\n', '\r':
	case '"':
		e.inStr = true
		e.role = roleSkip
		switch {
		case e.depth == 1 && e.expKey:
			e.role = roleKey
			e.keyBuf.Reset()
			e.expKey = false
		case e.depth == 1 && e.expValue:
			e.expValue = false
			if e.lastKey == e.key {
				e.role = roleValue
			}
		}
	case '{', '[':
		if e.depth == 0 {
			e.topObj = b == '{'
		}
		e.nonStringValue()
		e.depth++
		if e.depth == 1 && e.topObj {
			e.expKey = true
		}
	case '}', ']':
		if e.depth > 0 {
			e.depth--
		}
	case ':':
		if e.depth == 1 {
			e.expValue = true
		}
	case ',':
		if e.depth == 1 {
			e.expKey = e.topObj
			e.expValue = false
		}
	default:
		// A literal (number, true, false, null) — or garbage, which the provider's
		// final arguments will expose; nothing here depends on it.
		e.nonStringValue()
	}
}

// nonStringValue handles a value token that is not a string at depth 1.
func (e *Extractor) nonStringValue() {
	if e.depth == 1 && e.expValue {
		e.expValue = false
		if e.lastKey == e.key {
			e.done = true
		}
	}
}

func (e *Extractor) stringByte(b byte) {
	if e.uLeft > 0 {
		v, ok := hexVal(b)
		if !ok {
			// A malformed \u escape: give up on it rather than guess.
			e.uLeft = 0
			e.writeRune(utf8.RuneError)
			return
		}
		e.uVal = e.uVal<<4 | v
		e.uLeft--
		if e.uLeft == 0 {
			e.codepoint(e.uVal)
		}
		return
	}
	if e.esc {
		e.esc = false
		switch b {
		case 'u':
			e.uLeft, e.uVal = 4, 0
		case 'n':
			e.writeRune('\n')
		case 't':
			e.writeRune('\t')
		case 'r':
			e.writeRune('\r')
		case 'b':
			e.writeRune('\b')
		case 'f':
			e.writeRune('\f')
		default: // '"', '\\', '/' and anything else stand for themselves
			e.writeRune(rune(b))
		}
		return
	}
	switch b {
	case '\\':
		e.esc = true
	case '"':
		e.flushSurrogate()
		e.inStr = false
		switch e.role {
		case roleKey:
			e.lastKey = e.keyBuf.String()
		case roleValue:
			e.done = true
		}
	default:
		e.flushSurrogate()
		e.writeByte(b)
	}
}

// codepoint places one \u value, pairing surrogates.
func (e *Extractor) codepoint(r rune) {
	switch {
	case r >= 0xD800 && r < 0xDC00:
		e.flushSurrogate()
		e.hi = r
	case r >= 0xDC00 && r < 0xE000:
		if e.hi != 0 {
			full := ((e.hi - 0xD800) << 10) + (r - 0xDC00) + 0x10000
			e.hi = 0
			e.put(full)
			return
		}
		e.put(utf8.RuneError)
	default:
		e.flushSurrogate()
		e.put(r)
	}
}

// writeRune places a decoded non-\u character.
func (e *Extractor) writeRune(r rune) {
	e.flushSurrogate()
	e.put(r)
}

// flushSurrogate resolves a high surrogate that no low surrogate followed.
func (e *Extractor) flushSurrogate() {
	if e.hi != 0 {
		e.hi = 0
		e.put(utf8.RuneError)
	}
}

func (e *Extractor) put(r rune) {
	switch e.role {
	case roleKey:
		e.keyBuf.WriteRune(r)
	case roleValue:
		e.out = utf8.AppendRune(e.out, r)
	}
}

func (e *Extractor) writeByte(b byte) {
	switch e.role {
	case roleKey:
		e.keyBuf.WriteByte(b)
	case roleValue:
		e.out = append(e.out, b)
	}
}

// drain returns the complete UTF-8 prefix of the pending output and keeps an
// incomplete trailing sequence for the next piece.
func (e *Extractor) drain() string {
	if len(e.out) == 0 {
		return ""
	}
	cut := len(e.out)
	for i := len(e.out) - 1; i >= 0 && i >= len(e.out)-utf8.UTFMax; i-- {
		if utf8.RuneStart(e.out[i]) {
			if !utf8.FullRune(e.out[i:]) {
				cut = i
			}
			break
		}
	}
	s := string(e.out[:cut])
	e.out = append(e.out[:0], e.out[cut:]...)
	if e.done && !e.inStr {
		// The value is closed: whatever tail remains can never complete.
		if len(e.out) > 0 {
			s += string(utf8.RuneError)
			e.out = e.out[:0]
		}
	}
	return s
}

func hexVal(b byte) (rune, bool) {
	switch {
	case b >= '0' && b <= '9':
		return rune(b - '0'), true
	case b >= 'a' && b <= 'f':
		return rune(b-'a') + 10, true
	case b >= 'A' && b <= 'F':
		return rune(b-'A') + 10, true
	}
	return 0, false
}
