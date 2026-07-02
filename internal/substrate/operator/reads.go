package operator

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

const defaultAuditQueryLimit = 200

// QueryAudit reads the operator audit log, filtered and paginated. Read RPC
// (any authenticated role); never part of Snapshot. ADR-0047 D8/0047-08.
func (s *Service) QueryAudit(ctx context.Context, req *pb.QueryAuditRequest) (*pb.QueryAuditResponse, error) {
	if s.audit == nil {
		return nil, status.Error(codes.Unimplemented, "operator audit store not configured")
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultAuditQueryLimit
	}
	rows, err := s.audit.Query(ctx, AuditFilter{
		Actor:      req.GetActor(),
		TargetType: req.GetTargetType(),
		TargetID:   req.GetTargetId(),
		ActionType: req.GetActionType(),
		Limit:      limit,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query audit: %v", err)
	}
	resp := &pb.QueryAuditResponse{}
	for _, r := range rows {
		resp.Entries = append(resp.Entries, auditEntryToOp(r))
	}
	return resp, nil
}

// AuditToJSON renders audit entries as a JSON array (export, ADR-0047 0047-08).
func AuditToJSON(entries []domain.AuditEntry) ([]byte, error) {
	return json.MarshalIndent(entries, "", "  ")
}

// AuditToCSV renders audit entries as CSV with a header row (export).
func AuditToCSV(entries []domain.AuditEntry) (string, error) {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"id", "command_id", "at", "actor", "role", "action_type", "target_type", "target_id", "before", "after", "reason", "result"})
	for _, e := range entries {
		_ = w.Write([]string{
			e.ID, e.CommandID, e.At.Format("2006-01-02T15:04:05Z07:00"), e.Actor, e.Role,
			e.ActionType, e.TargetType, e.TargetID, e.Before, e.After, e.Reason, e.Result,
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return b.String(), nil
}
