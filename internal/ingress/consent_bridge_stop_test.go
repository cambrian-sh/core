package ingress

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func stopBridgeWithin(t *testing.T, b *ConsentBridge) {
	t.Helper()
	done := make(chan struct{})
	go func() { b.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ConsentBridge.Stop did not return")
	}
}

func newStopTestBridge() *ConsentBridge {
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{ConsentTimeout: time.Second}).Consent()
	return NewConsentBridge(hub, &fakePoster{}, time.Second)
}

// Stop is safe without Start, twice, and a Start after Stop launches no render
// loop (goleak in TestMain checks)
// (T-20261118-kernel-shutdown-hangs-forever-when-the-kernel).
func TestConsentBridge_StopWithoutStart(t *testing.T) {
	b := newStopTestBridge()
	stopBridgeWithin(t, b)
	stopBridgeWithin(t, b)
	b.Start(context.Background())
	stopBridgeWithin(t, b)
}

// Start twice launches one render loop (a second would close done twice and
// panic); Stop waits for it to return, and a second Stop returns too.
func TestConsentBridge_StartTwiceStopTwice(t *testing.T) {
	b := newStopTestBridge()
	b.Start(context.Background())
	b.Start(context.Background())
	stopBridgeWithin(t, b)
	select {
	case <-b.done:
	default:
		t.Fatal("Stop returned before the render loop exited")
	}
	stopBridgeWithin(t, b)
}
