package operator_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/identity"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// T-20261118-operator-rpcs-answer-unauthenticated-invalid-or, end to end: a real
// gRPC server with the production operator interceptor, the real Service, the
// real Authenticator (identity.Chain → LocalAccounts → PgRegistry) and a real
// Postgres reached through a TCP proxy this test can cut. While the database is
// unreachable every operator call is refused as Unavailable (retryable) — never
// Unauthenticated, never let through — and the SAME token works again once the
// database is back. A token that is genuinely bad stays Unauthenticated.

// pgProxy forwards TCP to Postgres; while down it severs live connections and
// closes every new one at once, which is what pgx sees when the server stops.
type pgProxy struct {
	ln     net.Listener
	target string
	down   atomic.Bool
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
}

func newPGProxy(t *testing.T, target string) *pgProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &pgProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close(); p.sever() })
	return p
}

func (p *pgProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.down.Load() {
			_ = c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.track(c, up)
		go func() { _, _ = io.Copy(up, c); _ = up.Close(); _ = c.Close() }()
		go func() { _, _ = io.Copy(c, up); _ = up.Close(); _ = c.Close() }()
	}
}

func (p *pgProxy) track(cs ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range cs {
		p.conns[c] = struct{}{}
	}
}

func (p *pgProxy) sever() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

func (p *pgProxy) setDown(d bool) {
	p.down.Store(d)
	if d {
		p.sever()
	}
}

func TestOperatorPlane_PostgresDown_IsUnavailableThenRecovers(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t)
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newPGProxy(t, net.JoinHostPort(cfg.ConnConfig.Host, strconv.Itoa(int(cfg.ConnConfig.Port))))
	host, port, _ := net.SplitHostPort(proxy.ln.Addr().String())
	pn, _ := strconv.Atoi(port)
	cfg.ConnConfig.Host, cfg.ConnConfig.Port = host, uint16(pn)
	cfg.ConnConfig.ConnectTimeout = 2 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reg, err := identity.NewPgRegistry(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	const user = "prtest-eng2120-op"
	t.Cleanup(func() {
		proxy.setDown(false)
		_, _ = pool.Exec(context.Background(), `DELETE FROM principals WHERE id = $1`, user)
	})
	la := identity.NewLocalAccounts(reg, reg)
	if _, err := la.EnsureAccount(ctx, user, "s3cret", []string{"operator"}); err != nil {
		t.Fatal(err)
	}
	idp := identity.NewChain(la, nil)

	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetIdentity(idp)
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(operator.UnaryAuthInterceptor(idp, nil, nil)))
	pb.RegisterOperatorConsoleServer(srv, svc)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewOperatorConsoleClient(conn)

	lr, err := client.Login(ctx, &pb.LoginRequest{Username: user, Password: "s3cret"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	authed := func(tok string) context.Context {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		t.Cleanup(cancel)
		return metadata.AppendToOutgoingContext(c, "authorization", "Bearer "+tok)
	}
	who, err := client.WhoAmI(authed(lr.GetToken()), &pb.WhoAmIRequest{})
	if err != nil || who.GetPrincipal() != domain.UserPrincipal(user).String() {
		t.Fatalf("whoami before the outage: %+v %v", who, err)
	}

	// Postgres goes away.
	proxy.setDown(true)
	for i := 0; i < 3; i++ {
		_, err := client.WhoAmI(authed(lr.GetToken()), &pb.WhoAmIRequest{})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("call %d during the outage: %v, want Unavailable (a good token must not read as a bad one)", i, err)
		}
	}
	// A password sign-in during the outage is retryable too, and is not counted
	// against the source (no lockout after the outage).
	for i := 0; i < 6; i++ {
		if _, err := client.Login(ctx, &pb.LoginRequest{Username: user, Password: "s3cret"}); status.Code(err) != codes.Unavailable {
			t.Fatalf("login %d during the outage: %v, want Unavailable", i, err)
		}
	}
	// A token the kernel never issued is still a bad token.
	if _, err := client.WhoAmI(authed("never-issued"), &pb.WhoAmIRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unknown token during the outage: %v, want Unauthenticated", err)
	}

	// Postgres is back: the SAME token works, and login is not locked out.
	proxy.setDown(false)
	deadline := time.Now().Add(15 * time.Second)
	for {
		who, err = client.WhoAmI(authed(lr.GetToken()), &pb.WhoAmIRequest{})
		if err == nil || time.Now().After(deadline) || status.Code(err) != codes.Unavailable {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || who.GetPrincipal() != domain.UserPrincipal(user).String() {
		t.Fatalf("same token after recovery: %+v %v", who, err)
	}
	if _, err := client.Login(ctx, &pb.LoginRequest{Username: user, Password: "s3cret"}); err != nil {
		t.Fatalf("login after recovery: %v", err)
	}
	if _, err := client.WhoAmI(authed("never-issued"), &pb.WhoAmIRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unknown token after recovery: %v, want Unauthenticated", err)
	}
}

// Unit: the interceptor's mapping of every Resolve outcome, through
// operator.UnaryAuthInterceptor with an Authenticator returning each error.
type errAuthenticator struct{ err error }

func (e errAuthenticator) Resolve(context.Context, string) (domain.Identity, error) {
	return domain.Identity{}, e.err
}
func (errAuthenticator) Revoke(context.Context, string) error { return nil }

func TestAuthInterceptor_ResolveErrorClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want codes.Code
	}{
		"store down":            {errors.Join(domain.ErrAuthUnavailable, errors.New("connection refused")), codes.Unavailable},
		"bad token":             {domain.ErrUnauthenticated, codes.Unauthenticated},
		"unclassified error":    {errors.New("plugin said no"), codes.Unauthenticated},
		"zero principal no err": {nil, codes.Unauthenticated},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			_, err := operator.UnaryAuthInterceptor(errAuthenticator{tc.err}, nil, nil)(ctxWithToken("t"), nil,
				&grpc.UnaryServerInfo{FullMethod: "/cambrian.OperatorConsole/Snapshot"},
				func(context.Context, any) (any, error) { called = true; return nil, nil })
			if called {
				t.Fatal("the handler ran: the gate must fail closed")
			}
			if status.Code(err) != tc.want {
				t.Fatalf("code = %v (%v), want %v", status.Code(err), err, tc.want)
			}
		})
	}
	// The caller's own cancellation is reported as such, not as an auth verdict.
	cctx, cancel := context.WithCancel(ctxWithToken("t"))
	cancel()
	_, err := operator.UnaryAuthInterceptor(errAuthenticator{context.Canceled}, nil, nil)(cctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/cambrian.OperatorConsole/Snapshot"},
		func(context.Context, any) (any, error) { t.Fatal("handler ran"); return nil, nil })
	if status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled caller: %v, want Canceled", err)
	}
}
