package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// errBlockedHost is returned when the SSRF guard refuses a target.
var errBlockedHost = errors.New("host blocked by SSRF guard")

// HTTPSource deterministically probes an HTTP(S) endpoint (ADR-0078 D2): a bounded,
// redirect-free GET reporting status, content-type, size, and — for a JSON body that
// looks like an OpenAPI/Swagger doc — the API title/version. READ-ONLY (GET only).
//
// Security: probing a URL lifted from an untrusted request is an SSRF surface (ADR-0051
// D6/D13). The guard resolves the host and refuses loopback/private/link-local/unspecified
// targets unless AllowPrivate is set (dev opt-in for localhost APIs), and redirects are
// never followed (a public URL cannot bounce to an internal one). This is the deterministic
// stand-in for the ADR-0043 EgressAuditor path the LLM scout used.
type HTTPSource struct {
	Client       *http.Client
	MaxBytes     int64 // response read cap (0 ⇒ 64 KiB)
	AllowPrivate bool  // permit loopback/private/link-local hosts (dev only)
}

// NewHTTPSource builds a source with a redirect-free client and a bounded timeout.
func NewHTTPSource(allowPrivate bool) *HTTPSource {
	return &HTTPSource{
		Client: &http.Client{
			Timeout:       4 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		MaxBytes:     64 << 10,
		AllowPrivate: allowPrivate,
	}
}

func (s *HTTPSource) Kind() string { return "http" }

// blockedIP reports whether ip must be refused (unless AllowPrivate).
func (s *HTTPSource) blockedIP(ip net.IP) bool {
	if s.AllowPrivate {
		return false
	}
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// guard validates the URL scheme and resolves the host, refusing blocked IPs.
func (s *HTTPSource) guard(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if s.blockedIP(ip) {
			return nil, errBlockedHost
		}
		return u, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if s.blockedIP(ip) {
			return nil, errBlockedHost // any resolved IP blocked ⇒ refuse the whole host
		}
	}
	return u, nil
}

func (s *HTTPSource) Probe(ctx context.Context, target domain.DiscoveryTarget) ([]domain.DiscoveredEntity, error) {
	u, err := s.guard(target.Ref)
	if err != nil {
		if errors.Is(err, errBlockedHost) {
			return []domain.DiscoveredEntity{{
				Kind: "url", ID: target.Ref, Exists: false, Summary: "blocked by SSRF guard (not observed)",
			}}, nil
		}
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, */*")
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	maxBytes := s.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	ct := resp.Header.Get("Content-Type")

	summary := fmt.Sprintf("HTTP %d; content-type=%s; %d bytes", resp.StatusCode, ct, len(body))
	kind := "url"
	if title, ver, ok := openAPIShape(ct, body); ok {
		kind = "api"
		summary = fmt.Sprintf("OpenAPI/Swagger: %q v%s (HTTP %d)", title, ver, resp.StatusCode)
	}
	return []domain.DiscoveredEntity{{
		Kind: kind, ID: target.Ref, Exists: resp.StatusCode < 400, Summary: summary,
	}}, nil
}

// openAPIShape detects an OpenAPI/Swagger JSON body and extracts its title+version.
func openAPIShape(contentType string, body []byte) (title, version string, ok bool) {
	if !strings.Contains(contentType, "json") {
		return "", "", false
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Swagger string `json:"swagger"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", false
	}
	if doc.OpenAPI == "" && doc.Swagger == "" {
		return "", "", false
	}
	title = doc.Info.Title
	if title == "" {
		title = "(untitled)"
	}
	return title, doc.Info.Version, true
}
