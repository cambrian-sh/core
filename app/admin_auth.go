package app

import (
	"net/http"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// The HTTP admin surface's credential (ADR-0130 D3).
//
// /v1/admin/* on the ingestion server mutates tool grants, scope profiles and
// write tags and triggers consolidation — operator-plane actions that, until
// 2026-10-31, nobody authenticated: the server bound every interface and the
// routes checked nothing (T-20261031-ingestion-http-server-exposes-unauthenticated).
// They now take the SAME operator bearer the gRPC console takes, resolved
// through the SAME OperatorIdentity instance, so there is one operator token
// vocabulary and one place it is minted (Login) and expires (ADR-0130 1f).
// Moving these routes onto OperatorConsole RPCs — where the scope mutation
// already exists — is contract bump 0096; this is the credential until then.

// requireOperatorBearer gates an HTTP handler on `Authorization: Bearer
// <operator token>` and the operator ROLE. A viewer is refused: every route
// behind this is a mutation, and the gRPC gate (operator.isOperatorOnly)
// refuses viewers those too.
//
// The 401 carries no detail: "no token", "unknown token" and "expired token"
// must be the same answer, as on the gRPC plane.
func requireOperatorBearer(idp operator.OperatorIdentity, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if idp == nil {
			http.Error(w, "operator identity not configured", http.StatusServiceUnavailable)
			return
		}
		scheme, token, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		if !found || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(token) == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		principal, role, err := idp.Resolve(strings.TrimSpace(token))
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if role != operator.RoleOperator {
			http.Error(w, "role "+string(role)+" may not use the admin surface", http.StatusForbidden)
			return
		}
		// The domain principal, as the gRPC interceptor sets it, so a mutation
		// made here is attributed to the same operator it would be over gRPC.
		next.ServeHTTP(w, r.WithContext(domain.WithPrincipal(r.Context(), domain.UserPrincipal(principal))))
	})
}
