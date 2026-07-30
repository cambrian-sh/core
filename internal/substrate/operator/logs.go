package operator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/pkg/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The kernel's own logs, on the operator plane (contract 0082).
//
// Both RPCs read the in-process retention ring. Two properties of that ring are
// carried into every response rather than left for a client to discover: it is
// BOUNDED, so it forgets, and it does not survive a restart. `LogWindowOp` says
// how far back it reaches and how much it has dropped, because a window with
// nothing behind it must never be presented as "nothing happened".
//
// Both are operator-only. See operatorOnlyReads in authz.go — a log line carries
// whatever the kernel wrote and bypasses the access-policy plane, so it is not
// filtered per principal the way a memory read is.

const (
	// defaultLogQueryLimit bounds a query that asks for everything. The window
	// holds tens of thousands of records and a console renders a screenful.
	defaultLogQueryLimit = 1000
	// maxLogQueryLimit is the ceiling a client cannot talk past.
	maxLogQueryLimit = 10_000
	// tailPollInterval is how often a live tail checks the ring.
	//
	// The ring has no subscription mechanism, and giving it one would mean
	// back-pressure and fan-out for a reader that is a human watching a screen.
	// A short poll over an in-memory slice is cheaper than the machinery it
	// replaces, and the cost of being wrong is a fraction of a second of latency.
	tailPollInterval = 300 * time.Millisecond
)

// SetLogRing installs the retention window these RPCs read.
//
// nil ⇒ both answer Unimplemented, which is the honest state for a kernel whose
// logger was never initialised through the app entrypoint (tests, embedders).
func (s *Service) SetLogRing(r *util.LogRing) { s.logs = r }

func (s *Service) logRing() (*util.LogRing, error) {
	if s.logs == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel retains no logs in process")
	}
	return s.logs, nil
}

// QueryLogs reads the retained window.
func (s *Service) QueryLogs(_ context.Context, req *pb.QueryLogsOpRequest) (*pb.QueryLogsOpResponse, error) {
	ring, err := s.logRing()
	if err != nil {
		return nil, err
	}

	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultLogQueryLimit
	}
	if limit > maxLogQueryLimit {
		limit = maxLogQueryLimit
	}

	f := compileLogFilter(req.GetFilter())
	// Read the whole window then filter: the ring is in memory and a predicate
	// over it is cheaper than maintaining an index for a screen that is opened
	// occasionally.
	all := ring.Since(req.GetAfterSeq(), 0)

	matched := make([]*pb.LogRecordOp, 0, min(limit, len(all)))
	// Newest-biased: when more matches than the limit, an operator wants the
	// recent ones. Walk backwards, then restore chronological order.
	for i := len(all) - 1; i >= 0 && len(matched) < limit; i-- {
		if f.matches(all[i]) {
			matched = append(matched, logRecordToProto(all[i]))
		}
	}
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}

	return &pb.QueryLogsOpResponse{Records: matched, Window: windowToProto(ring.Stats())}, nil
}

// TailLogs streams records as they are retained.
//
// `after_seq` of 0 starts at the LIVE EDGE rather than replaying the window: a
// tail is for what happens next, and QueryLogs is for history. A client that
// wants both asks for the history first and tails from the sequence it got.
func (s *Service) TailLogs(req *pb.TailLogsOpRequest, stream pb.OperatorConsole_TailLogsServer) error {
	ring, err := s.logRing()
	if err != nil {
		return err
	}

	cursor := req.GetAfterSeq()
	if cursor == 0 {
		cursor = ring.Stats().LastSeq
	}
	f := compileLogFilter(req.GetFilter())

	ticker := time.NewTicker(tailPollInterval)
	defer ticker.Stop()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			// The client went away. Not an error worth reporting — a tail ends
			// when the person watching closes the screen.
			return nil
		case <-ticker.C:
			batch := ring.Since(cursor, maxLogQueryLimit)
			for _, rec := range batch {
				// Advance on EVERY record, matched or not. Advancing only on
				// matches would re-scan the filtered-out ones forever, and a
				// selective filter would pin the cursor and burn the window.
				cursor = rec.Seq
				if !f.matches(rec) {
					continue
				}
				if err := stream.Send(logRecordToProto(rec)); err != nil {
					return err
				}
			}
		}
	}
}

// ── filtering ───────────────────────────────────────────────────────────────

type logFilter struct {
	floor      slog.Level
	components map[string]bool
	contains   string
	attrEquals map[string]string
	since      time.Time
}

func compileLogFilter(f *pb.LogFilterOp) logFilter {
	out := logFilter{floor: slog.LevelDebug}
	if f == nil {
		return out
	}
	if lvl, ok := parseLogLevel(f.GetLevelAtLeast()); ok {
		out.floor = lvl
	}
	if cs := f.GetComponents(); len(cs) > 0 {
		out.components = make(map[string]bool, len(cs))
		for _, c := range cs {
			out.components[c] = true
		}
	}
	out.contains = strings.ToLower(strings.TrimSpace(f.GetContains()))
	if ae := f.GetAttrEquals(); len(ae) > 0 {
		out.attrEquals = ae
	}
	if ms := f.GetSinceUnixMs(); ms > 0 {
		out.since = time.UnixMilli(ms)
	}
	return out
}

func (f logFilter) matches(r util.LogRecord) bool {
	if r.Level < f.floor {
		return false
	}
	if f.components != nil && !f.components[r.Component] {
		return false
	}
	if !f.since.IsZero() && r.At.Before(f.since) {
		return false
	}
	if f.contains != "" && !strings.Contains(strings.ToLower(r.Message), f.contains) {
		return false
	}
	for k, want := range f.attrEquals {
		got, present := r.Attrs[k]
		if !present || attrToString(got) != want {
			return false
		}
	}
	return true
}

func parseLogLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error", "err":
		return slog.LevelError, true
	}
	return slog.LevelDebug, false
}

func logLevelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}

// ── mapping ─────────────────────────────────────────────────────────────────

func logRecordToProto(r util.LogRecord) *pb.LogRecordOp {
	out := &pb.LogRecordOp{
		Seq:       r.Seq,
		AtUnixMs:  r.At.UnixMilli(),
		Level:     logLevelName(r.Level),
		Component: r.Component,
		Message:   r.Message,
		BootId:    r.BootID,
		Truncated: r.Truncated,
	}
	for k, v := range r.Attrs {
		out.Attrs = append(out.Attrs, logAttrToProto(k, v))
	}
	return out
}

// logAttrToProto keeps a value's TYPE where the wire can carry it. Everything
// else is stringified — losing the type is better than dropping the attribute,
// but only as a last resort, because a stringified number cannot be compared.
func logAttrToProto(key string, v any) *pb.LogAttrOp {
	a := &pb.LogAttrOp{Key: key}
	switch t := v.(type) {
	case string:
		a.Value = &pb.LogAttrOp_S{S: t}
	case bool:
		a.Value = &pb.LogAttrOp_B{B: t}
	case int:
		a.Value = &pb.LogAttrOp_I{I: int64(t)}
	case int64:
		a.Value = &pb.LogAttrOp_I{I: t}
	case uint64:
		a.Value = &pb.LogAttrOp_I{I: int64(t)}
	case float64:
		a.Value = &pb.LogAttrOp_F{F: t}
	case time.Duration:
		a.Value = &pb.LogAttrOp_S{S: t.String()}
	case time.Time:
		a.Value = &pb.LogAttrOp_S{S: t.Format(time.RFC3339Nano)}
	default:
		a.Value = &pb.LogAttrOp_S{S: fmt.Sprint(t)}
	}
	return a
}

// attrToString renders a value for `attr_equals`, which compares as text so a
// console's filter chip does not have to know the attribute's type.
func attrToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func windowToProto(st util.LogRingStats) *pb.LogWindowOp {
	w := &pb.LogWindowOp{
		BootId:   st.BootID,
		Capacity: int32(st.Capacity),
		Count:    int32(st.Count),
		Dropped:  st.Dropped,
		LastSeq:  st.LastSeq,
	}
	// A zero time is "no records", not 1970. Left at zero so a client can tell.
	if !st.Oldest.IsZero() {
		w.OldestUnixMs = st.Oldest.UnixMilli()
	}
	if !st.Newest.IsZero() {
		w.NewestUnixMs = st.Newest.UnixMilli()
	}
	return w
}
