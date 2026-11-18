package pyenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// State is the boot check's verdict on the env (D7.11.2).
type State string

const (
	// StateHealthy: current.json names a generation built from this release's
	// inputs, its marker and interpreter are intact and the probe passes.
	StateHealthy State = "healthy"
	// StateStale: a healthy generation built from other inputs (an older pin
	// table or base lock). Usable — running agents keep it — until the new
	// generation is built and switched to (D7.10.4).
	StateStale State = "stale"
	// StateMissing: no current generation (never built, or current.json gone).
	StateMissing State = "missing"
	// StateBroken: the current generation is damaged (files deleted, marker or
	// interpreter digest wrong, probe fails).
	StateBroken State = "broken"
)

// Status is one boot check result.
type Status struct {
	State       State  `json:"state"`
	Generation  string `json:"generation,omitempty"`  // directory name under base/
	Interpreter string `json:"interpreter,omitempty"` // absolute path
	Reason      string `json:"reason,omitempty"`
	Packages    int    `json:"packages,omitempty"`
}

// Usable reports whether Python agents may run on the env as it is.
func (s Status) Usable() bool { return s.State == StateHealthy || s.State == StateStale }

// Check runs the boot check against want (this release's inputs): current.json
// present, the generation's marker readable and matching its directory, the
// interpreter present with the digest recorded at build, and the probe
// (python -I -c "import cambrian_agent_sdk") passing.
func (b *Builder) Check(ctx context.Context) (Status, error) {
	want, _, err := b.Inputs()
	if err != nil {
		return Status{}, err
	}
	var cur Current
	if err := readJSON(filepath.Join(b.Root, fileCurrent), &cur); err != nil {
		if errIsNotExist(err) {
			return Status{State: StateMissing, Reason: "no sandbox Python env under " + b.Root}, nil
		}
		return Status{State: StateBroken, Reason: "current.json unreadable: " + err.Error()}, nil
	}
	if cur.Base == "" {
		return Status{State: StateBroken, Reason: "current.json names no base generation"}, nil
	}
	return inspect(ctx, b.runner(), b.Root, cur.Base, want), nil
}

// inspect checks one base generation; want decides healthy vs stale.
func inspect(ctx context.Context, run runFunc, root, dir string, want Inputs) Status {
	gen := filepath.Join(root, dirBase, dir)
	st := Status{Generation: dir}
	if fi, err := os.Stat(gen); err != nil || !fi.IsDir() {
		st.State, st.Reason = StateBroken, fmt.Sprintf("base generation %s is missing", dir)
		return st
	}
	var m Marker
	if err := readJSON(filepath.Join(gen, fileMarker), &m); err != nil {
		st.State, st.Reason = StateBroken, fmt.Sprintf("base generation %s: marker unreadable (%v)", dir, err)
		return st
	}
	if m.Kind != "base" || len(m.ID) < 16 || m.ID[:16] != dir || m.Inputs.ID() != m.ID {
		st.State, st.Reason = StateBroken, fmt.Sprintf("base generation %s: marker does not match its directory", dir)
		return st
	}
	st.Packages = m.Packages
	interp := filepath.Join(gen, filepath.FromSlash(m.Interpreter))
	st.Interpreter = interp
	sum, err := fileSHA256(interp)
	if err != nil {
		st.State, st.Reason = StateBroken, fmt.Sprintf("base generation %s: interpreter missing (%v)", dir, err)
		return st
	}
	if sum != m.InterpreterSHA256 {
		st.State, st.Reason = StateBroken, fmt.Sprintf("base generation %s: interpreter digest %s differs from the %s recorded at build", dir, sum, m.InterpreterSHA256)
		return st
	}
	if err := probe(ctx, run, interp); err != nil {
		st.State, st.Reason = StateBroken, fmt.Sprintf("base generation %s: %v", dir, err)
		return st
	}
	// A dev SDK wheel is a setup-time choice, not a release input: a generation
	// that differs from want only by it is current, not stale.
	got := m.Inputs
	got.SDKWheelSHA256 = want.SDKWheelSHA256
	if got != want {
		st.State, st.Reason = StateStale, fmt.Sprintf("base generation %s was built from other inputs (python %s, lock %s…); this release pins python %s, lock %s…",
			dir, m.Inputs.Python.Name, short(m.Inputs.LockSHA256), want.Python.Name, short(want.LockSHA256))
		return st
	}
	st.State = StateHealthy
	return st
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
