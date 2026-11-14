package postgres

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// seesReviewCandidates reports whether this read may include candidate
// (unreviewed) links (ENG-1536, owner decision 2026-11-14): only an OPERATOR on
// the operator plane sees candidates — they are the review lane's to judge;
// agent reads, in OSS and premium alike, see confirmed links only. Keyed on the
// surface the request arrived through (stamped server-side by the surface
// interceptor, never a caller field) AND an unrestricted read, not on Bypass
// alone: the OSS AllowAllAuthorizer hands every principal Bypass, so Bypass
// cannot mean "operator". A request with no surface (an in-process kernel path)
// sees confirmed links only — fail closed.
func seesReviewCandidates(ctx context.Context, scope *domain.TagPredicate) bool {
	return scope != nil && scope.Bypass && domain.SurfaceFromContext(ctx).Kind == domain.SurfaceOperator
}
