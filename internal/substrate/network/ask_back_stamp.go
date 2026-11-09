package network

import (
	"strconv"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// stampAskBack writes the ADR-0145 D5a resume keys on an Execute request from the
// caller's turn lease binding (LeaseBinding.AskBackRound / AskBackPendingRef,
// which only the chat lane sets), after deleting whatever the caller sent under
// them: handoff keys are kernel-written, and an agent must not be able to claim a
// round (and so skip the ask-back limit) or point the run at another pending
// state. resolve is the server's binding resolution for the request.
func stampAskBack(in *pb.Handoff, resolve func(map[string]string) (domain.LeaseBinding, bool)) {
	if in == nil {
		return
	}
	if in.Metadata == nil {
		in.Metadata = map[string]string{}
	}
	delete(in.Metadata, domain.HandoffKeyAskBackRound)
	delete(in.Metadata, domain.HandoffKeyAskBackPendingRef)
	b, known := resolve(in.Metadata)
	if !known || b.AskBackRound <= 0 {
		return
	}
	in.Metadata[domain.HandoffKeyAskBackRound] = strconv.Itoa(b.AskBackRound)
	if b.AskBackPendingRef != "" {
		in.Metadata[domain.HandoffKeyAskBackPendingRef] = b.AskBackPendingRef
	}
}
