package app

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type recordingLoginObs struct{ seen []domain.LoginAttempt }

func (r *recordingLoginObs) ObserveLogin(a domain.LoginAttempt) { r.seen = append(r.seen, a) }

type loginObsPlugin struct {
	name string
	obs  domain.LoginObserver
}

func (p *loginObsPlugin) Manifest() PluginManifest {
	return PluginManifest{ID: p.name, DisplayName: p.name, Version: "test"}
}

func (p *loginObsPlugin) Register(r *Registry) error {
	r.AddLoginObserver(p.obs)
	return nil
}

// ADR-0130 1f: a plugin-registered login observer must reach Options, fanned
// out with a directly-set one, or failed logins stay WARN-log-only (the gap
// T-20261101-failed-operator-logins-reach-only-the-warn-log recorded).
func TestLoginObserver_ReachesOptionsAndFansOut(t *testing.T) {
	direct, a, b := &recordingLoginObs{}, &recordingLoginObs{}, &recordingLoginObs{}
	composed, err := applyPlugins(Options{
		LoginObserver: direct,
		Plugins:       []Plugin{&loginObsPlugin{name: "receipts", obs: a}, &loginObsPlugin{name: "monitor", obs: b}},
	})
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if composed.opts.LoginObserver == nil {
		t.Fatal("registered login observers never reached Options")
	}
	composed.opts.LoginObserver.ObserveLogin(domain.LoginAttempt{Username: "op", Failures: 1})
	for name, r := range map[string]*recordingLoginObs{"direct": direct, "a": a, "b": b} {
		if len(r.seen) != 1 || r.seen[0].Username != "op" {
			t.Errorf("observer %s saw %+v, want one attempt", name, r.seen)
		}
	}
}

func TestLoginObserver_NoneRegisteredStaysNil(t *testing.T) {
	composed, err := applyPlugins(Options{})
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if composed.opts.LoginObserver != nil {
		t.Fatal("OSS default must be nil so Login stays one nil check")
	}
}
