package domain

import "context"

// SceneWriter persists a shallow MnemonicScene document after a successful step
// and writes a specifies edge to the prior step's scene.
// ADR-0025: nil = no scene written (zero behaviour change for existing callers).
type SceneWriter interface {
	WriteScene(ctx context.Context, result StepResult) (sceneID string, err error)
}
