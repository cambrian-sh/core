package executer

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// filterSnapshotForStep builds the context snapshot passed to a step.
// It applies the DependsOn graph as the sole routing authority (ADR-0022 Phase 0):
//
//   - step_N_result keys pass only for N declared in step.DependsOn
//   - step_N_{k} agent-added metadata keys are always stripped
//   - all non-step keys (ltm_*, initialContext, substrate metadata) always pass
//
// The returned map is a fresh copy — mutations do not affect masterContext.
func filterSnapshotForStep(master map[string]string, step domain.Step) map[string]string {
	filtered := make(map[string]string, len(master))

	// Index the declared dependencies for O(1) lookup.
	allowed := make(map[int]bool, len(step.DependsOn))
	for _, idx := range step.DependsOn {
		allowed[idx] = true
	}

	for k, v := range master {
		if !strings.HasPrefix(k, "step_") {
			// Non-step key: always passes through.
			filtered[k] = v
			continue
		}
		// Parse step_N_result — the only step-prefixed key that can pass.
		var idx int
		var suffix string
		if n, _ := fmt.Sscanf(k, "step_%d_%s", &idx, &suffix); n == 2 && suffix == "result" && allowed[idx] {
			filtered[k] = v
		}
		// step_N_{k} agent metadata keys and step_N_checkpoint are intentionally
		// excluded — they caused context pollution and the planner's DependsOn
		// does not express a dependency on metadata annotations.
	}

	keysStripped := len(master) - len(filtered)
	slog.Info("executor_context_filter",
		"depend_count", len(step.DependsOn),
		"keys_passed", len(filtered),
		"keys_stripped", keysStripped,
		"prior_total", len(master),
	)

	return filtered
}
