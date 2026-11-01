package app

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// ADR-0130 gate criterion 3: every http.Server the kernel or a plugin builds is
// built by listener_security.go. This walks the core tree and, when the
// premium sibling is checked out beside it, the premium tree, and refuses any
// `http.Server{` literal in a non-test file outside the policy file.
//
// The allowlist below is the RESIDUAL the 2026-10-31 implementation could not
// reach (files owned by other work, or binaries that cannot import app without
// a cycle). Each entry names the ticket that retires it; an entry with no
// ticket is a defect, and so is adding one without a ticket.
var listenerPolicyResidual = map[string]string{
	// internal/health cannot import app (app imports internal/*); the shim's
	// server is constructed there. T-20261101-remaining-http-server-literals-outside-listener.
	"internal/health/httpshim.go": "T-20261101-remaining-http-server-literals-outside-listener",
	// Operator-run maintenance binary, not a kernel listener (AGENTS.md §6.1 shape).
	"cmd/pagerank-recompute/main.go": "T-20261101-remaining-http-server-literals-outside-listener",
	// Premium: the ingress-studio webhook receiver and a dev viewer binary.
	"ingressstudio/transport_webhook.go": "T-20261101-remaining-http-server-literals-outside-listener",
	"cmd/cambrian-runview/main.go":       "T-20261101-remaining-http-server-literals-outside-listener",
}

func TestListenerPolicy_OnlyListenerSecurityBuildsHTTPServers(t *testing.T) {
	t.Parallel()
	roots := map[string]string{"core": ".."}
	if premium := filepath.Join("..", "..", "cambrian-premium"); dirExists(premium) {
		roots["premium"] = premium
	}
	var offenders []string
	for name, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				base := d.Name()
				if base == ".git" || base == "node_modules" || base == "vendor" || base == "internal_benchmarks" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !strings.Contains(string(src), "http.Server{") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if rel == "app/listener_security.go" {
				return nil
			}
			// A comment MENTIONING the literal is not a server (mcp_endpoint.go's
			// history note). Count only code lines.
			if !codeMentions(string(src), "http.Server{") {
				return nil
			}
			if _, ok := listenerPolicyResidual[rel]; ok {
				return nil
			}
			offenders = append(offenders, name+":"+rel)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", name, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("http.Server built outside app/listener_security.go (ADR-0130 D3; use serveHTTP / app.ServeHTTPListener):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// codeMentions reports whether needle appears on a line that is not a comment.
func codeMentions(src, needle string) bool {
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// serveHTTP binds on the kernel's bind address (loopback by default), sets the
// timeouts and caps every body — the shape every listener inherits.
func TestServeHTTP_LoopbackDefaultTimeoutsAndBodyCap(t *testing.T) {
	cfg := cfgWith("", "", "", false)
	var gotBody int
	var gotErr error
	srv, addr, err := serveHTTP(cfg, "0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, rerr := io.ReadAll(r.Body)
		gotBody, gotErr = len(b), rerr
		w.WriteHeader(http.StatusOK)
	}), "test")
	if err != nil {
		t.Fatalf("serveHTTP: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("bound %q, want the loopback default", addr)
	}
	if srv.ReadHeaderTimeout != httpReadHeaderTimeout || srv.IdleTimeout != httpIdleTimeout {
		t.Fatalf("timeouts not applied: header=%v idle=%v", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
	if srv.Addr != "" {
		t.Fatalf("server carries its own Addr %q — the listener, not the server, decides the bind", srv.Addr)
	}
	// A body past the cap is cut off by MaxBytesReader.
	big := strings.NewReader(strings.Repeat("x", int(defaultHTTPMaxBodyBytes)+1))
	resp, err := http.Post("http://"+addr+"/", "application/octet-stream", big)
	if err == nil {
		resp.Body.Close()
	}
	// Either the client sees the connection closed or the handler saw the
	// MaxBytesError; both prove the cap. What must NOT happen is the handler
	// reading the whole body without error.
	if gotErr == nil && gotBody > int(defaultHTTPMaxBodyBytes) {
		t.Fatalf("handler read %d bytes with no error — body cap not applied", gotBody)
	}
}

// A routable bind without TLS or the opt-in is refused BEFORE binding, so an
// HTTP listener can never be the one that quietly opens a plaintext port.
func TestServeHTTP_RefusesRoutablePlaintext(t *testing.T) {
	t.Parallel()
	_, _, err := serveHTTP(cfgWith("0.0.0.0", "", "", false), "0", http.NotFoundHandler(), "test")
	if err == nil || !strings.Contains(err.Error(), "PLAINTEXT") {
		t.Fatalf("routable plaintext HTTP listener was not refused: %v", err)
	}
}

// The plugin seam answers a named error before Run attaches the policy, and
// serves exactly like serveHTTP after.
func TestServeHTTPListener_RequiresAttachedPolicy(t *testing.T) {
	setListenerPolicy(nil)
	if _, err := ServeHTTPListener("0", http.NotFoundHandler(), "plugin"); err != ErrListenerPolicyUnavailable {
		t.Fatalf("err = %v, want ErrListenerPolicyUnavailable", err)
	}
	setListenerPolicy(cfgWith("", "", "", false))
	t.Cleanup(func() { setListenerPolicy(nil) })
	srv, err := ServeHTTPListener("0", http.NotFoundHandler(), "plugin")
	if err != nil {
		t.Fatalf("ServeHTTPListener: %v", err)
	}
	_ = srv.Shutdown(context.Background())
}

// /v1/admin/* takes the operator bearer through the SAME identity the gRPC
// interceptor resolves, refuses viewers, and says nothing distinguishing about
// a missing versus a wrong token.
func TestRequireOperatorBearer(t *testing.T) {
	t.Parallel()
	idp := operator.NewStaticIdentity(map[string]struct {
		Password string
		Role     operator.Role
	}{
		"op":     {Password: "pw", Role: operator.RoleOperator},
		"viewer": {Password: "pw", Role: operator.RoleViewer},
	})
	opTok, _, _, err := idp.Login("op", "pw")
	if err != nil {
		t.Fatal(err)
	}
	viewTok, _, _, err := idp.Login("viewer", "pw")
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	h := requireOperatorBearer(idp, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called++ }))
	cases := []struct {
		name, auth string
		want       int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"not bearer", "Basic abc", http.StatusUnauthorized},
		{"unknown token", "Bearer nope", http.StatusUnauthorized},
		{"viewer", "Bearer " + viewTok, http.StatusForbidden},
		{"operator", "Bearer " + opTok, http.StatusOK},
		{"operator, scheme case-insensitive", "bearer " + opTok, http.StatusOK},
	}
	for _, c := range cases {
		rec := &recorder{header: http.Header{}}
		req, _ := http.NewRequest(http.MethodPost, "/v1/admin/consolidate", nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		h.ServeHTTP(rec, req)
		if rec.code() != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.code(), c.want)
		}
	}
	if called != 2 {
		t.Fatalf("handler ran %d times, want 2 (the two operator calls)", called)
	}
}

// recorder is a minimal ResponseWriter (httptest is avoided in package app to
// keep its test binary free of the net/http/httptest server type).
type recorder struct {
	header http.Header
	status int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(code int)        { r.status = code }
func (r *recorder) code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
