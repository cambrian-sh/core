package objects

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// The read side's only I/O (ADR-0149 D8: validate → plan → encode are pure;
// exec runs the one statement). List and Get take the caller's predicate as a
// required parameter; the principal → predicate step is the kernel's
// (authz.ScopedObjects), never a plugin's.

// StatementTimeout bounds every statement the reader runs (ADR-0149 D6
// principle 6: SET LOCAL statement_timeout per query). A fixed bound beside
// the request bounds (page size, hops, buckets), not a tunable.
const StatementTimeout = 5 * time.Second

// Reader answers objects requests over the object tables.
type Reader struct {
	pool      *pgxpool.Pool
	kinds     domain.KindSource
	relations domain.RelationSource
}

// NewReader builds the reader over the live declarations (the same sources the
// maintainer and the generator read).
func NewReader(pool *pgxpool.Pool, kinds domain.KindSource, relations domain.RelationSource) *Reader {
	return &Reader{pool: pool, kinds: kinds, relations: relations}
}

// List answers one objects request (ADR-0149 D6) as the holder of scope.
func (r *Reader) List(ctx context.Context, scope *domain.TagPredicate, q domain.ObjectsQuery) (domain.ObjectsResult, error) {
	if scope == nil {
		return domain.ObjectsResult{}, domain.ErrQueryScopeMissing
	}
	c, err := validate(q, r.kinds, r.relations)
	if err != nil {
		return domain.ObjectsResult{}, err
	}
	out := domain.ObjectsResult{Kind: c.Spec.Kind, DeclVersion: c.DeclVersion}
	if c.Group {
		err = r.read(ctx, func(tx pgx.Tx) error {
			vis, rt, err := visibleClasses(ctx, tx, c, scope)
			if err != nil {
				return err
			}
			sql, args := planGroup(c, scope, vis, rt)
			out.Groups, out.Truncated, err = scanGroups(ctx, tx, c, sql, args)
			return err
		})
		return out, err
	}
	cur, err := decodeToken(q, c)
	if err != nil {
		return domain.ObjectsResult{}, err
	}
	err = r.read(ctx, func(tx pgx.Tx) error {
		vis, rt, err := visibleClasses(ctx, tx, c, scope)
		if err != nil {
			return err
		}
		sql, args := planList(c, scope, cur, vis, rt)
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var lastKeys []any
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return err
			}
			if len(out.Rows) == c.PageSize {
				out.NextPageToken = encodeToken(q, c, lastKeys, out.Rows[len(out.Rows)-1].EntityID)
				break
			}
			id, _ := vals[0].(string)
			lastKeys = vals[1 : 1+len(c.OrderBy)]
			row := domain.ObjectRow{EntityID: id}
			for i, f := range c.Fields {
				if v, ok := typedValue(f, vals[1+len(c.OrderBy)+i]); ok {
					row.Values = append(row.Values, domain.ObjectValue{Property: f.Name, Value: v})
				}
			}
			out.Rows = append(out.Rows, row)
		}
		return rows.Err()
	})
	return out, err
}

// visibleClasses reads, in the request's snapshot, which row classifications
// it holds (planRowTagged; for a filtered caller only — ScopeMaintenance renders
// no fence) and the caller's visible fence classes V (planVisible) when the
// request needs the existence rule (nil otherwise).
func visibleClasses(ctx context.Context, tx pgx.Tx, c checked, scope *domain.TagPredicate) ([]int32, rowTagged, error) {
	var rt rowTagged
	if scope.Unfiltered() {
		return nil, rt, nil
	}
	if err := tx.QueryRow(ctx, planRowTagged(c)).Scan(&rt.Values, &rt.Links); err != nil {
		return nil, rt, err
	}
	if !needsExistence(c, scope) {
		return nil, rt, nil
	}
	sql, args := planVisible(scope, rt.Values)
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, rt, err
	}
	vis, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return nil, rt, err
	}
	if vis == nil {
		vis = []int32{} // binds '{}' (no class visible), never NULL
	}
	return vis, rt, nil
}

// read runs fn in a read-only transaction under the statement timeout.
//
// REPEATABLE READ: a request may run up to three statements (the row-tags
// probe, the visible classes, then the query), and all must see one snapshot,
// so a row rewritten between them is judged by the classes of the same moment,
// and a row-classified row the query sees is one the probe saw (ADR-0158; the
// plan renders the row-tags fence only when the probe found one). A read-only transaction never
// fails serialization.
//
// jit = off: the planner's cost estimate for a fenced statement is inflated by
// the fence's correlated SubPlans and crosses the JIT thresholds, and JIT
// compile time then dominated (3.3 s of 5.8 s for an unfiltered group at 10^5,
// investigations/2026-11-13-unfiltered-listobjects-existence-cost.md). The
// statements here are short, bounded and index-driven: compilation never pays
// back.
//
// plan_cache_mode = force_custom_plan: the existence rule binds the caller's
// visible classes as an array, and Postgres hashes `x = ANY(array)` only when
// the array is a constant at planning time — a custom plan. pgx prepares and
// caches statements, and after five executions Postgres may switch to a
// generic plan in which the array is a parameter and every row scans it
// linearly: at 10^5 orders an unfiltered group took 588 ms instead of 229 with
// 49 visible classes and 15.3 s instead of 264 ms with 20,049
// (T-20261113-unfiltered-listobjects-group-list-under-a).
func (r *Reader) read(ctx context.Context, fn func(pgx.Tx) error) error {
	opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := pgx.BeginTxFunc(ctx, r.pool, opts, func(tx pgx.Tx) error {
		// One round trip: Exec with no arguments uses the simple protocol.
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d; SET LOCAL jit = off; "+
			"SET LOCAL plan_cache_mode = force_custom_plan", StatementTimeout.Milliseconds())); err != nil {
			return err
		}
		return fn(tx)
	})
	if err != nil {
		return fmt.Errorf("objects: read: %w", err)
	}
	return nil
}

func scanGroups(ctx context.Context, tx pgx.Tx, c checked, sql string, args []any) ([]domain.ObjectGroup, bool, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []domain.ObjectGroup
	truncated := false
	for rows.Next() {
		if len(out) == domain.ObjectsMaxGroups {
			truncated = true
			break
		}
		vals, err := rows.Values()
		if err != nil {
			return nil, false, err
		}
		var g domain.ObjectGroup
		for i, p := range c.GroupBy {
			v, _ := typedValue(p, vals[i])
			g.Key = append(g.Key, domain.ObjectValue{Property: p.Name, Value: v})
		}
		for i, m := range c.Metrics {
			raw := vals[len(c.GroupBy)+i]
			var v domain.StatementValue
			switch {
			case m.Prop == nil:
				n, _ := raw.(int64)
				v = domain.StatementValue{Type: "number", Number: float64(n)}
			case m.Fn == domain.ObjectMetricMin || m.Fn == domain.ObjectMetricMax:
				v, _ = typedValue(*m.Prop, raw)
			default:
				v, _ = typedValue(prop{Name: m.Prop.Name, Type: "number"}, raw)
			}
			g.Metrics = append(g.Metrics, v)
		}
		out = append(out, g)
	}
	return out, truncated, rows.Err()
}

// typedValue converts a scanned column value of p to a typed value; false for
// NULL (absent or not visible).
func typedValue(p prop, raw any) (domain.StatementValue, bool) {
	v := domain.StatementValue{Predicate: p.Name, Type: p.Type}
	switch x := raw.(type) {
	case nil:
		return domain.StatementValue{}, false
	case float64:
		v.Number = x
	case time.Time:
		v.Date = x.UTC()
	case string:
		if p.Type == "entity" {
			v.EntityRef = x
		} else {
			v.Text = x
		}
	default:
		return domain.StatementValue{}, false
	}
	return v, true
}
