package network

import (
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// instanceWorkLog remembers, per kernel-spawned agent instance, the binding of
// the last lease that instance presented and the kernel resolved for it
// (ADR-0157 D5.2). The instance id comes from the verified per-instance token
// (domain.AgentInstanceFromContext), never from a request. It is how an upload
// without a lease header, and an instance's out/ collected at exit, still name
// the run, session, step and conversation they were produced for. An entry is
// dropped when the instance's out/ is collected (the instance is gone).
type instanceWorkLog struct {
	mu   sync.Mutex
	last map[string]domain.LeaseBinding
}

// instanceWork returns the server's log, created on first use.
func (s *Server) instanceWork() *instanceWorkLog {
	s.instWorkOnce.Do(func() { s.instWork = &instanceWorkLog{last: map[string]domain.LeaseBinding{}} })
	return s.instWork
}

func (l *instanceWorkLog) note(instanceID string, b domain.LeaseBinding) {
	if instanceID == "" {
		return
	}
	l.mu.Lock()
	l.last[instanceID] = b
	l.mu.Unlock()
}

func (l *instanceWorkLog) lastFor(instanceID string) (domain.LeaseBinding, bool) {
	if instanceID == "" {
		return domain.LeaseBinding{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.last[instanceID]
	return b, ok
}

func (l *instanceWorkLog) forget(instanceID string) {
	l.mu.Lock()
	delete(l.last, instanceID)
	l.mu.Unlock()
}
