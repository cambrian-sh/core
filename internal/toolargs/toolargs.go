// Package toolargs validates a tool call's arguments against the tool's JSON
// Schema at the dispatch boundary, coercing what is unambiguous first (ADR-0145
// D9, execution.tools.validate_args; T-20261108-assistant-passes-mail-draft-to-as-a-json).
//
// The model proxy in front of Cambrian does not enforce tool schemas (strict is
// dropped), so a model can send {"to": "[\"a@x\"]"} — an array encoded as a
// string — and a world that stores the literal string then "sends" to it. The
// kernel is the only place every call passes, so it is checked here, in code:
//
//   - coercion, guided by the schema, of exactly these unambiguous mistakes: a
//     JSON array/object sent as a string where the schema wants that type; a
//     single value where it wants an array of that value's type; a numeric or
//     boolean string where it wants a number, integer or boolean; a number or
//     boolean where it wants a string;
//   - then full validation (github.com/santhosh-tekuri/jsonschema/v6, the
//     validator the provider layer already uses for response schemas), whose
//     failure is returned as an error naming the argument and what was expected,
//     for the model to correct. The call does not run.
package toolargs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/cambrian-sh/core/domain"
)

// Validator implements domain.ToolArgValidator.
type Validator struct{}

var _ domain.ToolArgValidator = Validator{}

var compiled sync.Map // sha256(schema) -> *jsonschema.Schema (or error)

type compileErr struct{ err error }

func compile(raw []byte) (*jsonschema.Schema, error) {
	key := sha256.Sum256(raw)
	if v, ok := compiled.Load(key); ok {
		if ce, isErr := v.(compileErr); isErr {
			return nil, ce.err
		}
		return v.(*jsonschema.Schema), nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		compiled.Store(key, compileErr{err})
		return nil, err
	}
	c := jsonschema.NewCompiler()
	const loc = "mem://tool-args.json"
	if err := c.AddResource(loc, doc); err != nil {
		compiled.Store(key, compileErr{err})
		return nil, err
	}
	s, err := c.Compile(loc)
	if err != nil {
		compiled.Store(key, compileErr{err})
		return nil, err
	}
	compiled.Store(key, s)
	return s, nil
}

// ValidateArgs coerces then validates args against schema. A schema the kernel
// cannot compile is not the caller's fault: the arguments pass unchanged (logged
// by the caller's audit as usual) rather than every call of the tool failing.
func (Validator) ValidateArgs(tool string, schema, args []byte) ([]byte, error) {
	var sch map[string]any
	if len(bytes.TrimSpace(schema)) == 0 || json.Unmarshal(schema, &sch) != nil {
		return args, nil
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = []byte("{}")
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return nil, fmt.Errorf("arguments for %s are not valid JSON (%v): send one JSON object", tool, err)
	}
	v, changed := coerce(v, sch)
	out := args
	if changed {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("arguments for %s: %v", tool, err)
		}
		out = b
	}
	s, err := compile(schema)
	if err != nil {
		return out, nil
	}
	if verr := s.Validate(v); verr != nil {
		return nil, fmt.Errorf("arguments for %s do not match its schema: %s. Correct the arguments and call it again", tool, describe(verr))
	}
	return out, nil
}

var printer = message.NewPrinter(language.English)

// describe renders a validation error as its leaf causes, one per argument.
func describe(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	var leaves []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			if loc == "/" {
				loc = "(the arguments)"
			}
			leaves = append(leaves, fmt.Sprintf("%s: %s", loc, e.ErrorKind.LocalizedString(printer)))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return strings.Join(leaves, "; ")
}

// types returns the schema's type set ("type": "x" or ["x","null"]).
func types(sch map[string]any) []string {
	switch t := sch["type"].(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func wants(sch map[string]any, t string) bool {
	for _, x := range types(sch) {
		if x == t {
			return true
		}
	}
	return false
}

// coerce rewrites v toward sch where the fix is unambiguous; changed reports any
// rewrite. It never drops or invents a value.
func coerce(v any, sch map[string]any) (any, bool) {
	if sch == nil {
		return v, false
	}
	ts := types(sch)
	if len(ts) == 0 {
		// No type: still descend into declared properties of an object.
		if m, ok := v.(map[string]any); ok {
			return coerceObject(m, sch)
		}
		return v, false
	}
	if matches(v, ts) {
		switch x := v.(type) {
		case map[string]any:
			return coerceObject(x, sch)
		case []any:
			return coerceArray(x, sch)
		}
		return v, false
	}
	switch x := v.(type) {
	case string:
		t := strings.TrimSpace(x)
		if (wants(sch, "array") && strings.HasPrefix(t, "[")) || (wants(sch, "object") && strings.HasPrefix(t, "{")) {
			var inner any
			if json.Unmarshal([]byte(t), &inner) == nil && matches(inner, ts) {
				out, _ := coerce(inner, sch)
				return out, true
			}
		}
		if wants(sch, "integer") {
			if n, err := strconv.ParseInt(t, 10, 64); err == nil {
				return float64(n), true
			}
		}
		if wants(sch, "number") {
			if f, err := strconv.ParseFloat(t, 64); err == nil {
				return f, true
			}
		}
		if wants(sch, "boolean") {
			if b, err := strconv.ParseBool(t); err == nil && (t == "true" || t == "false") {
				return b, true
			}
		}
	case float64:
		if wants(sch, "string") {
			return strconv.FormatFloat(x, 'f', -1, 64), true
		}
	case bool:
		if wants(sch, "string") {
			return strconv.FormatBool(x), true
		}
	}
	// A single value where an array of that value's type is wanted.
	if wants(sch, "array") {
		if _, isArr := v.([]any); !isArr && v != nil {
			items, _ := sch["items"].(map[string]any)
			if items == nil || len(types(items)) == 0 || matches(v, types(items)) {
				return []any{v}, true
			}
			if c, ok := coerce(v, items); ok && matches(c, types(items)) {
				return []any{c}, true
			}
		}
	}
	return v, false
}

func coerceObject(m map[string]any, sch map[string]any) (any, bool) {
	props, _ := sch["properties"].(map[string]any)
	changed := false
	for k, pv := range m {
		ps, _ := props[k].(map[string]any)
		if ps == nil {
			continue
		}
		if nv, ok := coerce(pv, ps); ok {
			m[k] = nv
			changed = true
		}
	}
	return m, changed
}

func coerceArray(a []any, sch map[string]any) (any, bool) {
	items, _ := sch["items"].(map[string]any)
	if items == nil {
		return a, false
	}
	changed := false
	for i, x := range a {
		if nv, ok := coerce(x, items); ok {
			a[i] = nv
			changed = true
		}
	}
	return a, changed
}

func matches(v any, ts []string) bool {
	for _, t := range ts {
		switch t {
		case "string":
			if _, ok := v.(string); ok {
				return true
			}
		case "number":
			if _, ok := v.(float64); ok {
				return true
			}
		case "integer":
			if f, ok := v.(float64); ok && f == float64(int64(f)) {
				return true
			}
		case "boolean":
			if _, ok := v.(bool); ok {
				return true
			}
		case "array":
			if _, ok := v.([]any); ok {
				return true
			}
		case "object":
			if _, ok := v.(map[string]any); ok {
				return true
			}
		case "null":
			if v == nil {
				return true
			}
		}
	}
	return false
}
