package operator

import "testing"

// The operator view of every principal's scheduled tasks is other people's
// business: a Viewer may not read it even though it is spelled like a read.
func TestListScheduledTasks_IsOperatorOnly(t *testing.T) {
	t.Parallel()
	if !isOperatorOnly("/cambrian.OperatorConsole/ListScheduledTasks") {
		t.Fatal("ListScheduledTasks must be operator-only")
	}
}
