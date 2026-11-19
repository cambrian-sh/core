package egressproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestParseAllowlistAndMatch(t *testing.T) {
	a, err := ParseAllowlist([]string{"api.example.com", "Files.Example.org:8443", "*.slack.com", "203.0.113.7", "[2001:db8::1]:9443"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"api.example.com", 443, true},
		{"API.EXAMPLE.COM.", 443, true},
		{"api.example.com", 80, false},
		{"evil-api.example.com", 443, false},
		{"api.example.com.evil.net", 443, false},
		{"files.example.org", 8443, true},
		{"files.example.org", 443, false},
		{"hooks.slack.com", 443, true},
		{"a.b.slack.com", 443, true},
		{"slack.com", 443, false}, // *.slack.com is strictly below
		{"203.0.113.7", 443, true},
		{"203.0.113.8", 443, false},
		{"2001:db8::1", 9443, true},
		{"2001:db8::1", 443, false},
		{"", 443, false},
		{"bad_name.example.com", 443, false},
	}
	for _, c := range cases {
		if got, _ := a.match(c.host, c.port); got != c.want {
			t.Errorf("match(%q, %d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
	for _, bad := range []string{"*", "", "*.com", "the configured database host", "https://api.example.com", "a.example.com:0", "a.example.com:x", "2001:db8::1:443x", "[::1"} {
		if _, err := ParseAllowlist([]string{bad}); err == nil {
			t.Errorf("ParseAllowlist(%q) accepted", bad)
		}
	}
}

func TestClassify(t *testing.T) {
	if ok, any, ph := Classify([]string{"pypi.org", "*.googleapis.com"}); !ok || any || ph != "" {
		t.Fatalf("plain list: %v %v %q", ok, any, ph)
	}
	if ok, any, _ := Classify([]string{"pypi.org", "*"}); ok || !any {
		t.Fatalf("star: %v %v", ok, any)
	}
	if ok, _, ph := Classify([]string{"registry.npmjs.org", "the configured SEARXNG_URL host"}); ok || ph != "the configured SEARXNG_URL host" {
		t.Fatalf("placeholder: %v %q", ok, ph)
	}
	if ok, any, ph := Classify(nil); ok || any || ph != "" {
		t.Fatal("empty list must not be enforceable")
	}
}

func TestForbiddenAddresses(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "127.9.9.9", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "0.1.2.3", "::", "224.0.0.1", "::ffff:127.0.0.1"} {
		if !forbidden(net.ParseIP(s)) {
			t.Errorf("%s must be forbidden", s)
		}
	}
	for _, s := range []string{"10.1.2.3", "192.168.1.1", "203.0.113.7", "2001:db8::1"} {
		if forbidden(net.ParseIP(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
}

// fakeDNS maps names to addresses; the rig's Dial maps the "public" address to
// a local echo server, so nothing leaves the machine.
type fakeDNS map[string][]string

func (f fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

type rig struct {
	broker  *Broker
	session *Session
	fwdAddr string
	dialed  chan string
	stop    func()
}

// newRig wires the whole path the sandbox uses: a forwarder listening on
// loopback, the multiplex over an in-memory pipe pair, and a Session of a
// started Broker whose upstream dials land on a local echo server.
func newRig(t *testing.T, hosts []string) *rig {
	t.Helper()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	r := &rig{dialed: make(chan string, 16)}
	r.broker = New(Options{
		Resolver: fakeDNS{"allowed.test": {"203.0.113.10"}, "api.wild.test": {"203.0.113.11"}, "rebind.test": {"127.0.0.1"}, "meta.test": {"169.254.169.254"}},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			r.dialed <- addr
			var d net.Dialer
			return d.DialContext(ctx, network, echo.Addr().String())
		},
	})
	if err := r.broker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.session, err = r.broker.Open("srv", hosts)
	if err != nil {
		t.Fatal(err)
	}
	kernelEnd, fwdEnd := net.Pipe()
	if err := r.session.Serve(kernelEnd); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.fwdAddr = ln.Addr().String()
	fwdDone := make(chan error, 1)
	go func() { fwdDone <- Forward(ln, fwdEnd) }()
	select {
	case <-r.session.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("forwarder never announced ready")
	}
	r.stop = func() {
		r.broker.Stop()
		<-fwdDone
		_ = echo.Close()
		<-echoDone
	}
	t.Cleanup(r.stop)
	return r
}

// connect sends one CONNECT through the forwarder and returns the status code
// and, when admitted, the tunnel.
func (r *rig) connect(t *testing.T, target string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", r.fwdAddr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = c.Close()
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, c, br
}

func TestProxyAdmitsDeclaredHostAndTunnels(t *testing.T) {
	r := newRig(t, []string{"allowed.test", "*.wild.test"})
	for _, target := range []string{"allowed.test:443", "api.wild.test:443"} {
		code, c, br := r.connect(t, target)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", target, code)
		}
		if _, err := io.WriteString(c, "ping\n"); err != nil {
			t.Fatal(err)
		}
		line, err := br.ReadString('\n')
		if err != nil || line != "ping\n" {
			t.Fatalf("%s: tunnel echo %q %v", target, line, err)
		}
		_ = c.Close()
	}
	// The resolved address was dialled, never the name.
	for _, want := range []string{"203.0.113.10:443", "203.0.113.11:443"} {
		if got := <-r.dialed; got != want {
			t.Fatalf("dialled %s, want %s", got, want)
		}
	}
	if a, _ := r.session.Counts(); a != 2 {
		t.Fatalf("admitted %d, want 2", a)
	}
}

func TestProxyRefusesUndeclaredLiteralsRebindingAndPlainHTTP(t *testing.T) {
	r := newRig(t, []string{"allowed.test", "rebind.test", "meta.test"})
	for target, want := range map[string]int{
		"evil.test:443":         http.StatusForbidden, // not declared
		"allowed.test:8443":     http.StatusForbidden, // declared name, undeclared port
		"203.0.113.10:443":      http.StatusForbidden, // the declared name's own address as a literal
		"[2001:db8::10]:443":    http.StatusForbidden,
		"rebind.test:443":       http.StatusForbidden, // declared name resolving to loopback
		"meta.test:443":         http.StatusForbidden, // declared name resolving to the metadata endpoint
		"localhost:443":         http.StatusForbidden,
		"allowed.test.evil:443": http.StatusForbidden,
	} {
		if code, _, _ := r.connect(t, target); code != want {
			t.Errorf("CONNECT %s: status %d, want %d", target, code, want)
		}
	}
	// A plain-HTTP proxy request is refused too.
	c, err := net.Dial("tcp", r.fwdAddr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(c, "GET http://allowed.test/ HTTP/1.1\r\nHost: allowed.test\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("plain GET: %v %v", resp, err)
	}
	_ = c.Close()
	select {
	case d := <-r.dialed:
		t.Fatalf("a refused request dialled %s", d)
	default:
	}
	if _, refused := r.session.Counts(); refused != 9 {
		t.Fatalf("refused %d, want 9", refused)
	}
}

func TestBrokerStopEndsLiveTunnels(t *testing.T) {
	r := newRig(t, []string{"allowed.test"})
	code, c, br := r.connect(t, "allowed.test:443")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	defer func() { _ = c.Close() }()
	r.broker.Stop() // the rig's cleanup Stop is then a no-op
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("tunnel still open after Stop")
	}
	if _, err := r.broker.Open("x", []string{"allowed.test"}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Open after Stop: %v", err)
	}
}

func TestMuxRejectsOversizedFrames(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveMux(a, nil, func(c net.Conn) { _ = c.Close() }) }()
	_, _ = b.Write([]byte{frameData, 0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff})
	if err := <-done; !errors.Is(err, ErrProtocol) {
		t.Fatalf("serveMux: %v, want ErrProtocol", err)
	}
	_ = b.Close()
}

// TestPythonForwarderSpeaksTheMux runs forwarder.py (the docker sidecar) on
// the host's Python against a real Session: the two implementations of the
// multiplex agree. Skipped when no Python 3 is on PATH, and on Windows (the
// sidecar runs in a Linux container; asyncio's Windows loop cannot read an
// anonymous pipe): there the docker runner's live e2e runs it instead.
func TestPythonForwarderSpeaksTheMux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("forwarder.py runs in a Linux container; covered on Windows by the docker live e2e")
	}
	py := ""
	for _, n := range []string{"python3", "python"} {
		if p, err := exec.LookPath(n); err == nil {
			py = p
			break
		}
	}
	if py == "" {
		t.Skip("no python on PATH")
	}
	r := newRig(t, []string{"allowed.test"})
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	sess, err := r.broker.Open("py", []string{"allowed.test"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-I", "-c", PythonForwarder, strconv.Itoa(port)) //nolint:gosec // test
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := sess.Serve(pipeRWC{stdout, stdin}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Ready():
	case <-time.After(20 * time.Second):
		t.Fatal("python forwarder never announced ready")
	}
	pr := &rig{fwdAddr: "127.0.0.1:" + strconv.Itoa(port)}
	code, c, br := pr.connect(t, "allowed.test:443")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	payload := strings.Repeat("x", 200000) + "\n" // several frames each way
	go func() { _, _ = io.WriteString(c, payload) }()
	got, err := br.ReadString('\n')
	if err != nil || got != payload {
		t.Fatalf("echo through the python forwarder: %d bytes, %v", len(got), err)
	}
	_ = c.Close()
	if code, _, _ := pr.connect(t, "evil.test:443"); code != http.StatusForbidden {
		t.Fatalf("undeclared through python forwarder: %d", code)
	}
	sess.Close()
}

type pipeRWC struct {
	io.ReadCloser
	io.WriteCloser
}

func (p pipeRWC) Close() error {
	_ = p.WriteCloser.Close()
	return p.ReadCloser.Close()
}
