package network

import (
	"context"
	"fmt"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RegisterWatch creates or replaces a WatchConfig. Premium build only. ADR-0032.
func (s *Server) RegisterWatch(ctx context.Context, req *pb.RegisterWatchRequest) (*pb.RegisterWatchResponse, error) {
	if s.WatchHandler == nil {
		return nil, status.Error(codes.Unimplemented, "RegisterWatch: WatchHandler not configured")
	}
	if req.Config == nil {
		return nil, status.Error(codes.InvalidArgument, "RegisterWatch: config is required")
	}
	cfg := protoToWatchConfig(req.Config)
	id, err := s.WatchHandler.RegisterWatch(cfg)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "RegisterWatch: %v", err)
	}
	return &pb.RegisterWatchResponse{Id: id}, nil
}

// ListWatches returns all registered WatchConfigs. Premium build only. ADR-0032.
func (s *Server) ListWatches(ctx context.Context, _ *pb.ListWatchesRequest) (*pb.ListWatchesResponse, error) {
	if s.WatchHandler == nil {
		return nil, status.Error(codes.Unimplemented, "ListWatches: WatchHandler not configured")
	}
	configs, err := s.WatchHandler.ListWatches()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ListWatches: %v", err)
	}
	protos := make([]*pb.WatchConfigProto, 0, len(configs))
	for _, cfg := range configs {
		c := cfg
		protos = append(protos, watchConfigToProto(&c))
	}
	return &pb.ListWatchesResponse{Configs: protos}, nil
}

// DeleteWatch removes a WatchConfig by ID. Premium build only. ADR-0032.
func (s *Server) DeleteWatch(_ context.Context, req *pb.DeleteWatchRequest) (*pb.DeleteWatchResponse, error) {
	if s.WatchHandler == nil {
		return nil, status.Error(codes.Unimplemented, "DeleteWatch: WatchHandler not configured")
	}
	if err := s.WatchHandler.DeleteWatch(req.Id); err != nil {
		return nil, status.Errorf(codes.NotFound, "DeleteWatch: %v", err)
	}
	return &pb.DeleteWatchResponse{Id: req.Id}, nil
}

// SetWatchActive enables or disables a WatchConfig without deleting it. Premium build only. ADR-0032.
func (s *Server) SetWatchActive(_ context.Context, req *pb.SetWatchActiveRequest) (*pb.SetWatchActiveResponse, error) {
	if s.WatchHandler == nil {
		return nil, status.Error(codes.Unimplemented, "SetWatchActive: WatchHandler not configured")
	}
	if err := s.WatchHandler.SetWatchActive(req.Id, req.Active); err != nil {
		return nil, status.Errorf(codes.NotFound, "SetWatchActive: %v", err)
	}
	return &pb.SetWatchActiveResponse{Id: req.Id, Active: req.Active}, nil
}

// ── proto ↔ domain mappers ─────────────────────────────────────────────────

func protoToWatchConfig(p *pb.WatchConfigProto) domain.WatchConfig {
	cfg := domain.WatchConfig{
		ID:                 p.Id,
		Name:               p.Name,
		Description:        p.Description,
		Condition:          p.Condition,
		ConditionType:      p.ConditionType,
		Active:             p.Active,
		ResponseMode:       p.ResponseMode,
		MaxConcurrentPlans: int(p.MaxConcurrentPlans),
		Source: domain.WatchSource{
			Type:     p.SourceType,
			StreamID: p.SourceStreamId,
		},
	}
	if p.Action != nil {
		cfg.Action = domain.WatchAction{
			Type:       p.Action.Type,
			TargetType: p.Action.TargetType,
			Target:     p.Action.Target,
			Payload:    p.Action.Payload,
		}
	}
	// DaemonParams: proto uses map<string,string>; domain uses map[string]any.
	// String-only values for now. ADR-0032 Note.
	if len(p.DaemonParams) > 0 {
		cfg.DaemonParams = make(map[string]any, len(p.DaemonParams))
		for k, v := range p.DaemonParams {
			cfg.DaemonParams[k] = v
		}
	}
	return cfg
}

func watchConfigToProto(cfg *domain.WatchConfig) *pb.WatchConfigProto {
	p := &pb.WatchConfigProto{
		Id:                 cfg.ID,
		Name:               cfg.Name,
		Description:        cfg.Description,
		Condition:          cfg.Condition,
		ConditionType:      cfg.ConditionType,
		Active:             cfg.Active,
		ResponseMode:       cfg.ResponseMode,
		MaxConcurrentPlans: int32(cfg.MaxConcurrentPlans),
		SourceType:         cfg.Source.Type,
		SourceStreamId:     cfg.Source.StreamID,
		Action: &pb.WatchActionProto{
			Type:       cfg.Action.Type,
			TargetType: cfg.Action.TargetType,
			Target:     cfg.Action.Target,
			Payload:    cfg.Action.Payload,
		},
	}
	if len(cfg.DaemonParams) > 0 {
		p.DaemonParams = make(map[string]string, len(cfg.DaemonParams))
		for k, v := range cfg.DaemonParams {
			p.DaemonParams[k] = fmt.Sprintf("%v", v)
		}
	}
	return p
}
