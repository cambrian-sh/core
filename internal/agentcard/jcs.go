package agentcard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// JSON Canonicalization Scheme, RFC 8785 (JCS).
//
// A2A 1.0 §8.4 signs an Agent Card over its JCS form, so the signer and every
// verifier must produce the same bytes from the same card. This is an in-house
// implementation rather than a dependency (ticket log
// T-20261105-adr-0139-d4-c2-generate-kernel-signed-a2a-agent): the rules are
// small and fully specified, the RFC ships test vectors (jcs_test.go holds
// §3.2.2, §3.2.3 and every Appendix B number row), and verification needs one
// property a generic canonicaliser does not give — DUPLICATE MEMBER NAMES ARE
// REFUSED. encoding/json keeps the last of two equal keys; another parser may
// keep the first. A card that two parsers read differently is a card whose
// signature covers something other than what the consumer acts on, so I-JSON's
// "names MUST be unique" (RFC 7493 §2.3, required by RFC 8785 §3.1) is enforced.
//
// What it does, per RFC 8785 §3.2:
//   - no insignificant whitespace;
//   - object members sorted by their names as arrays of UTF-16 code units;
//   - strings: only `"`, `\` and U+0000..U+001F are escaped (\b \t \n \f \r in
//     short form, the rest as lowercase \u00xx); everything else is literal UTF-8;
//   - numbers: IEEE-754 double, serialised as ECMAScript Number.prototype.toString
//     (shortest round-trip digits; exponent form below 1e-6 and from 1e21).
//
// Known limit: Go's decoder turns an escaped lone surrogate (\ud800) into U+FFFD
// rather than failing; I-JSON forbids it, and such a card canonicalises to the
// replacement character. Raw invalid UTF-8 is refused up front.

// ErrDuplicateKey reports an object with two members of the same name.
var ErrDuplicateKey = errors.New("jcs: duplicate object member name")

// Canonicalize returns the RFC 8785 canonical form of one JSON text.
func Canonicalize(in []byte) ([]byte, error) {
	if !utf8.Valid(in) {
		return nil, errors.New("jcs: input is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("jcs: trailing data after the JSON value")
	}
	var buf bytes.Buffer
	if err := writeValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CanonicalizeValue marshals v with encoding/json and canonicalises the result.
func CanonicalizeValue(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("jcs: marshal: %w", err)
	}
	return Canonicalize(b)
}

// member is one object member, kept in a slice so duplicates are detectable.
type member struct {
	name  string
	value any
}

type object []member

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("jcs: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			var obj object
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("jcs: %w", err)
				}
				name, ok := kt.(string)
				if !ok {
					return nil, errors.New("jcs: object member name is not a string")
				}
				if seen[name] {
					return nil, fmt.Errorf("%w: %q", ErrDuplicateKey, name)
				}
				seen[name] = true
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				obj = append(obj, member{name: name, value: v})
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, fmt.Errorf("jcs: %w", err)
			}
			if obj == nil {
				obj = object{}
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, fmt.Errorf("jcs: %w", err)
			}
			return arr, nil
		}
		return nil, fmt.Errorf("jcs: unexpected delimiter %q", t)
	default:
		return tok, nil // string, json.Number, bool, nil
	}
}

func writeValue(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeString(buf, x)
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			// Out of the IEEE-754 double range: not I-JSON.
			return fmt.Errorf("jcs: number %s is not representable as a double", x)
		}
		s, err := FormatNumber(f)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case object:
		sorted := append(object(nil), x...)
		sort.Slice(sorted, func(i, j int) bool { return lessUTF16(sorted[i].name, sorted[j].name) })
		buf.WriteByte('{')
		for i, m := range sorted {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, m.name)
			buf.WriteByte(':')
			if err := writeValue(buf, m.value); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("jcs: unsupported value %T", v)
	}
	return nil
}

// lessUTF16 orders two names by their UTF-16 code units (RFC 8785 §3.2.3).
// Comparing Go strings (UTF-8 bytes) differs for characters above U+FFFF
// against U+E000..U+FFFF, which is exactly what the RFC's sorting vector tests.
func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

func writeString(buf *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hex[r>>4])
				buf.WriteByte(hex[r&0xf])
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// FormatNumber serialises a double as ECMAScript's Number.prototype.toString
// does (ECMA-262 Number::toString, the RFC 8785 §3.2.2.3 rule). NaN and the
// infinities are not JSON and are refused.
func FormatNumber(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("jcs: %v is not a JSON number", f)
	}
	if f == 0 {
		return "0", nil // both zeros
	}
	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}
	// Shortest round-trip digits, as strconv's 'e' with precision -1 gives:
	// "d.ddddde±XX". digits is the significand without the point; n is the
	// position of the decimal point relative to the digits (ECMA-262's n).
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expPart, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, err := strconv.Atoi(expPart)
	if err != nil {
		return "", fmt.Errorf("jcs: format %v: %w", f, err)
	}
	k := len(digits)
	n := exp + 1
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		ex := n - 1
		es := "+"
		if ex < 0 {
			es = "-"
			ex = -ex
		}
		if k == 1 {
			out = digits + "e" + es + strconv.Itoa(ex)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + es + strconv.Itoa(ex)
		}
	}
	return sign + out, nil
}
