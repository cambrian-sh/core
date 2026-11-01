package app

import (
	"strings"
	"testing"

	"google.golang.org/grpc"
)

// grpcPlugin mounts one named gRPC service through AddGRPCService (ADR-0073).
type grpcPlugin struct {
	name    string
	service string
}

func (p *grpcPlugin) Manifest() PluginManifest {
	return PluginManifest{ID: p.name, DisplayName: p.name, Version: "test"}
}

func (p *grpcPlugin) Register(r *Registry) error {
	svc := p.service
	r.AddGRPCService(func(s *grpc.Server) {
		s.RegisterService(&grpc.ServiceDesc{ServiceName: svc, HandlerType: (*any)(nil)}, nil)
	})
	return nil
}

// A service mounted on the operator plane outside `cambrian.premium.` would be
// passed through the operator auth interceptor unauthenticated; composing it
// is a startup ERROR naming the service
// (T-20261031-extraservices-mounts-outside-package-cambrian).
func TestApplyPlugins_RefusesOperatorPlaneServiceOutsidePremiumPackage(t *testing.T) {
	_, err := applyPlugins(Options{Plugins: []Plugin{&grpcPlugin{name: "rogue", service: "acme.rogue.Service"}}})
	if err == nil {
		t.Fatal("a service outside cambrian.premium.* must fail composition")
	}
	if !strings.Contains(err.Error(), "acme.rogue.Service") || !strings.Contains(err.Error(), "UNAUTHENTICATED") {
		t.Fatalf("the error must name the service and the consequence, got: %v", err)
	}
}

// The premium package passes, and the composed registrar still mounts it.
func TestApplyPlugins_MountsPremiumPackageService(t *testing.T) {
	c, err := applyPlugins(Options{Plugins: []Plugin{&grpcPlugin{name: "ok", service: "cambrian.premium.test.Service"}}})
	if err != nil {
		t.Fatalf("applyPlugins: %v", err)
	}
	if c.opts.ExtraServices == nil {
		t.Fatal("ExtraServices must be composed from the plugin's registrar")
	}
	s := grpc.NewServer()
	defer s.Stop()
	c.opts.ExtraServices(s)
	if _, ok := s.GetServiceInfo()["cambrian.premium.test.Service"]; !ok {
		t.Fatalf("the premium service was not mounted: %v", s.GetServiceInfo())
	}
}
