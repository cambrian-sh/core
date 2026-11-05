package agentprofile

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// This file is the Go port of the strict YAML subset the SDK parses profiles with
// (sdk/cambrian_agent_sdk/prompt_library.py parse_frontmatter, _scalar,
// _flow_list; prompts/SPEC.md §2.2). It is ported rather than replaced by
// yaml.v3 on purpose: yaml.v3 accepts files the subset refuses (multi-line
// strings, anchors, nested maps, an unquoted value containing ": "), and a
// profile the kernel registered but the runtime then refused at boot would be an
// agent that is routed to and never answers. One subset, two ports, held
// together by testdata/profile_cases.json (TestProfileCasesFixture here,
// sdk/tests/test_profile_parity.py there).

var subsetInt = regexp.MustCompile(`^-?[0-9]+$`)

// null is the subset's null value (Python None): `~`, `null`, an empty value
// inside a flow list, or a bare `key:` with no block items.
type null struct{}

// parseSubset parses the profile text (already CRLF-normalised) into key → value,
// where a value is string, int64, null, []any (of string/int64/null), or any
// JSON value for an inline `{...}` object.
func parseSubset(front string) (map[string]any, []string, error) {
	out := map[string]any{}
	var order []string
	blockKey := ""
	inBlock := false
	withItems := map[string]bool{}
	var blockKeys []string
	for i, line := range strings.Split(front, "\n") {
		n := i + 1
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimLeft(line, " \t\n\r\v\f"), "#") {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			item := strings.TrimSpace(line)
			if !inBlock || !strings.HasPrefix(item, "- ") {
				return nil, nil, fmt.Errorf("frontmatter line %d: unexpected indentation", n)
			}
			v, err := subsetScalar(item[2:])
			if err != nil {
				return nil, nil, err
			}
			out[blockKey] = append(out[blockKey].([]any), v)
			withItems[blockKey] = true
			continue
		}
		key, value, sep := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if !sep || key == "" {
			return nil, nil, fmt.Errorf("frontmatter line %d: expected 'key: value'", n)
		}
		if _, dup := out[key]; dup {
			return nil, nil, fmt.Errorf("frontmatter key %q appears twice", key)
		}
		order = append(order, key)
		value = strings.TrimSpace(value)
		inBlock = false
		switch {
		case value == "":
			out[key] = []any{}
			blockKey, inBlock = key, true
			blockKeys = append(blockKeys, key)
		case strings.HasPrefix(value, "["):
			if !strings.HasSuffix(value, "]") {
				return nil, nil, fmt.Errorf("frontmatter line %d: unterminated flow list", n)
			}
			l, err := subsetFlowList(value)
			if err != nil {
				return nil, nil, err
			}
			out[key] = l
		case strings.HasPrefix(value, "{"):
			var v any
			if err := json.Unmarshal([]byte(value), &v); err != nil {
				return nil, nil, fmt.Errorf("frontmatter line %d: inline object is not JSON: %v", n, err)
			}
			out[key] = v
		default:
			v, err := subsetScalar(value)
			if err != nil {
				return nil, nil, err
			}
			out[key] = v
		}
	}
	// A bare "key:" with no block items is null, as YAML reads it.
	for _, k := range blockKeys {
		if !withItems[k] {
			out[k] = null{}
		}
	}
	return out, order, nil
}

func subsetScalar(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return nil, fmt.Errorf("bad double-quoted scalar %q: %v", raw, err)
		}
		return s, nil
	}
	if strings.HasPrefix(raw, "'") {
		if len(raw) < 2 || !strings.HasSuffix(raw, "'") {
			return nil, fmt.Errorf("bad single-quoted scalar %q", raw)
		}
		return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), nil
	}
	if subsetInt.MatchString(raw) {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			// Python reads an arbitrarily large int; every profile int field is
			// range-checked, so an overflow saturates into "out of range".
			if strings.HasPrefix(raw, "-") {
				return int64(-1 << 63), nil
			}
			return int64(1<<63 - 1), nil
		}
		return v, nil
	}
	if raw == "" || raw == "~" || raw == "null" {
		return null{}, nil
	}
	if strings.ContainsAny(raw[:1], "&*!|>%@`") || strings.Contains(raw, ": ") || strings.Contains(raw, " #") {
		return nil, fmt.Errorf("scalar %q is outside the frontmatter subset (quote it)", raw)
	}
	return raw, nil
}

func subsetFlowList(raw string) ([]any, error) {
	raw = strings.TrimSpace(raw)
	inner := strings.TrimSpace(raw[1 : len(raw)-1])
	if inner == "" {
		return []any{}, nil
	}
	parts := strings.Split(inner, ",")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		v, err := subsetScalar(p)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// pyStr renders a subset value the way Python's str() does, for the duplicate
// check (`len(set(map(str, v)))`).
func pyStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case null:
		return "None"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
