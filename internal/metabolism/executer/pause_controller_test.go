package executer_test

import (
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

func TestPauseController_ResumeUnblocks(t *testing.T) {
	pc := executer.NewPauseController()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pc.Wait()
	}()

	// Pause it, then immediately resume.
	pc.Pause()
	go func() {
		time.Sleep(5 * time.Millisecond)
		pc.Resume()
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// success — goroutine unblocked
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Wait() did not unblock after Resume()")
	}
}

func TestPauseController_AbortUnblocks(t *testing.T) {
	pc := executer.NewPauseController()
	pc.Pause()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pc.Wait()
	}()

	time.Sleep(5 * time.Millisecond)
	pc.Abort()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Wait() did not unblock after Abort()")
	}
}

func TestPauseController_IsAborted_AfterAbort(t *testing.T) {
	pc := executer.NewPauseController()
	pc.Pause()
	pc.Abort()
	if !pc.IsAborted() {
		t.Error("expected IsAborted() == true after Abort()")
	}
}

func TestPauseController_IsAborted_AfterResume(t *testing.T) {
	pc := executer.NewPauseController()
	pc.Pause()
	pc.Resume()
	if pc.IsAborted() {
		t.Error("expected IsAborted() == false after Resume()")
	}
}

func TestPauseController_NoPausePassesThrough(t *testing.T) {
	pc := executer.NewPauseController()
	// Not paused — Wait() must return immediately.
	done := make(chan struct{})
	go func() {
		pc.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Wait() blocked when not paused")
	}
}

func TestPauseController_NeedsIntervention_Destructive(t *testing.T) {
	pc := executer.NewPauseController()
	if !pc.NeedsIntervention("rm -rf /tmp/cache") {
		t.Error("expected destructive step to need intervention")
	}
}

func TestPauseController_NeedsIntervention_Safe(t *testing.T) {
	pc := executer.NewPauseController()
	if pc.NeedsIntervention("fetch config from API") {
		t.Error("expected safe step to not need intervention")
	}
}
