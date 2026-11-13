package objects

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Page tokens (ADR-0149 D6; Google AIP-158). Opaque to callers: base64url of
// the request's hash, the last row's sort values and its entity id. A token is
// bound to the request it came from — every parameter but page_size and the
// token itself — and refused under any other (ErrObjectsInvalid).
//
// A token is not a capability: a forged one only moves where the page starts.
// Every value in it is bound as a parameter and the fence applies as on page 1.

type tokenBody struct {
	Hash string `json:"h"`
	Keys []any  `json:"k"`
	ID   string `json:"id"`
}

// requestHash fingerprints the parameters a token is bound to.
func requestHash(q domain.ObjectsQuery) string {
	q.Namespace = orDefault(q.Namespace)
	q.PageToken, q.PageSize = "", 0
	raw, _ := json.Marshal(q) // the request is plain data; it always marshals
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

// encodeToken renders the cursor after the last row of a page.
func encodeToken(q domain.ObjectsQuery, c checked, keys []any, id string) string {
	out := make([]any, len(keys))
	for i, k := range keys {
		if t, ok := k.(time.Time); ok {
			out[i] = t.UTC().Format(time.RFC3339Nano)
			continue
		}
		out[i] = k
	}
	raw, _ := json.Marshal(tokenBody{Hash: requestHash(q), Keys: out, ID: id}) // plain data
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeToken parses q.PageToken against the validated request; nil for none.
func decodeToken(q domain.ObjectsQuery, c checked) (*cursor, error) {
	if q.PageToken == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(q.PageToken)
	if err != nil {
		return nil, invalid("page_token is malformed")
	}
	var tb tokenBody
	if err := json.Unmarshal(raw, &tb); err != nil || tb.ID == "" {
		return nil, invalid("page_token is malformed")
	}
	if tb.Hash != requestHash(q) {
		return nil, invalid("page_token was issued for a different request (only page_size may change between pages)")
	}
	if len(tb.Keys) != len(c.OrderBy) {
		return nil, invalid("page_token is malformed")
	}
	cur := &cursor{ID: tb.ID, Keys: make([]any, len(tb.Keys))}
	for i, k := range tb.Keys {
		if k == nil {
			continue
		}
		v, ok := tokenKey(c.OrderBy[i].Prop.Type, k)
		if !ok {
			return nil, invalid("page_token is malformed")
		}
		cur.Keys[i] = v
	}
	return cur, nil
}

// tokenKey converts one JSON-decoded sort value back to its column's type.
func tokenKey(typ string, k any) (any, bool) {
	switch typ {
	case "number":
		f, ok := k.(float64)
		return f, ok
	case "date":
		s, ok := k.(string)
		if !ok {
			return nil, false
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		return t.UTC(), err == nil
	default:
		s, ok := k.(string)
		return s, ok
	}
}
