package executer

import (
	"strings"
	"sync"
	"unicode"
)

// PauseController provides a pause/resume/abort gate for DAGExecutor.
// When a step requires human-in-the-loop confirmation, the executor calls
// Pause() then Wait(). The TUI calls Resume() or Abort() based on user input.
type PauseController struct {
	mu      sync.Mutex
	cond    *sync.Cond
	paused  bool
	aborted bool
}

// NewPauseController returns an unpaused controller.
func NewPauseController() *PauseController {
	pc := &PauseController{}
	pc.cond = sync.NewCond(&pc.mu)
	return pc
}

// Pause marks the controller as paused. Subsequent calls to Wait() will block.
func (pc *PauseController) Pause() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.paused = true
	pc.aborted = false
}

// Wait blocks until the controller is no longer paused (Resume or Abort called).
// Returns immediately if not paused.
func (pc *PauseController) Wait() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for pc.paused {
		pc.cond.Wait()
	}
}

// Resume unblocks any goroutines waiting in Wait(). Not aborted.
func (pc *PauseController) Resume() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.paused = false
	pc.aborted = false
	pc.cond.Broadcast()
}

// Abort unblocks waiting goroutines and marks the execution as aborted.
func (pc *PauseController) Abort() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.paused = false
	pc.aborted = true
	pc.cond.Broadcast()
}

// IsAborted reports whether Abort() was the last signal sent.
func (pc *PauseController) IsAborted() bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.aborted
}

// destructiveVerbs mirrors the shell.IsDestructive set for the substrate layer.
var destructiveVerbs = []string{"rm", "drop", "delete", "truncate", "format", "wipe", "destroy", "kill"}

// NeedsIntervention reports whether a step description contains a destructive verb
// (word-boundary aware). Used by DAGExecutor to decide whether to call Pause().
func (pc *PauseController) NeedsIntervention(description string) bool {
	lower := strings.ToLower(description)
	for _, verb := range destructiveVerbs {
		idx := 0
		for {
			pos := strings.Index(lower[idx:], verb)
			if pos < 0 {
				break
			}
			abs := idx + pos
			before := abs == 0 || !unicode.IsLetter(rune(lower[abs-1]))
			after := abs+len(verb) >= len(lower) || !unicode.IsLetter(rune(lower[abs+len(verb)]))
			if before && after {
				return true
			}
			idx = abs + 1
		}
	}
	return false
}
