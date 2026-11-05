package prompts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Pair is one organ call assembled from two library prompts (SPEC.md §2.3):
// `<id>`, the stable system block — no slots, so it is byte-identical on every
// call and the provider can cache it — and `<id>_input`, the volatile user
// message whose slots carry the per-call data inside tagged regions.
//
// It returns plain strings rather than an LLM request so this package keeps
// importing nothing from the kernel; the caller puts System into a cached system
// block, Message into the user turn and Schema into the response schema.
type Pair struct {
	System Prompt
	Input  Prompt
}

// LoadPair loads `<systemID>` and `<inputID>` from the embedded library and
// checks the system prompt has no slots (a slot there would make the cached
// prefix differ per call).
func LoadPair(systemID, inputID string) (Pair, error) {
	sys, err := Get(systemID)
	if err != nil {
		return Pair{}, err
	}
	in, err := Get(inputID)
	if err != nil {
		return Pair{}, err
	}
	if len(sys.Inputs) != 0 {
		return Pair{}, fmt.Errorf("%w: %s is a system block and must have no slots, has %v", ErrInvalid, systemID, sys.Inputs)
	}
	return Pair{System: sys, Input: in}, nil
}

// MustPair is LoadPair for package-level prompts: the files are embedded and
// linted, so a failure is a build defect, not a runtime condition.
func MustPair(systemID, inputID string) Pair {
	p, err := LoadPair(systemID, inputID)
	if err != nil {
		panic(fmt.Sprintf("prompts: %s + %s: %v", systemID, inputID, err))
	}
	return p
}

// StaticHash is the identity of the static text the pair sends: sha256 over the
// two prompts' static hashes. It changes whenever a sent static byte of either
// file changes, and it is the same for every render.
func (p Pair) StaticHash() string {
	sum := sha256.Sum256([]byte(p.System.StaticHash() + "\n" + p.Input.StaticHash()))
	return hex.EncodeToString(sum[:])
}

// Schema is the system prompt's response schema, or nil.
func (p Pair) Schema() []byte { return p.System.ResponseSchema }

// Message renders the user message from the per-call values. Values are
// untrusted data: the caller neutralises their closing tags first
// (NeutraliseClosingTags).
func (p Pair) Message(values map[string]string) (string, error) {
	r, err := p.Input.Render(values)
	if err != nil {
		return "", err
	}
	return r.Text, nil
}
