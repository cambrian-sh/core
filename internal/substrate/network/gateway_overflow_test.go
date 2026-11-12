package network

import (
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-worker-resolve-cid-inline-has-no-size-cap-a: the gateway used to
// answer a context overflow as Internal (its default), which the self-healer
// retries; OutOfRange is the deterministic "this request does not fit".
func TestGatewayStatus_ContextOverflowIsOutOfRange(t *testing.T) {
	err := gatewayStatus(fmt.Errorf("openai: %w: received error while streaming: Prompt is too long", domain.ErrContextOverflow))
	if got := status.Code(err); got != codes.OutOfRange {
		t.Fatalf("code = %v, want OutOfRange", got)
	}
}
