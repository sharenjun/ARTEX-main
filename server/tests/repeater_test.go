//go:build ignore

package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestValidateRepeaterRequestRejectsUnsafeInputs(t *testing.T) {
	base := repeaterRequest{TaskID: "1", Method: "GET", URL: "https://example.test/path", HTTPVersion: "HTTP/1.1"}
	if _, _, err := validateRepeaterRequest(base); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	for _, input := range []repeaterRequest{
		{TaskID: "1", Method: "GET", URL: "http://user:pass@example.test/"},
		{TaskID: "1", Method: "GET", URL: "http://example.test/#fragment"},
		{TaskID: "1", Method: "CONNECT", URL: "http://example.test/"},
		{TaskID: "1", Method: "GET", URL: "http://example.test/", HTTPVersion: "HTTP/1.0"},
		{TaskID: "1", Method: "GET", URL: "http://example.test/", UseRecordingProxy: true},
	} {
		if _, _, err := validateRepeaterRequest(input); err == nil {
			t.Fatalf("unsafe request was accepted: %#v", input)
		}
	}
}

func TestRepeaterPolicyRejectsReservedAndOutOfScopeAddresses(t *testing.T) {
	p := repeaterPolicy{scopes: []db.TaskScope{{Kind: "root_domain", Domain: "example.test"}}}
	u, _ := url.Parse("https://example.test/")
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254", "224.0.0.1"} {
		addr := netip.MustParseAddr(ip)
		if err := p.permits(u, addr); err == nil {
			t.Fatalf("reserved address %s was accepted", ip)
		}
	}
	other, _ := url.Parse("https://other.test/")
	if err := p.permits(other, netip.MustParseAddr("203.0.113.10")); err == nil {
		t.Fatal("target outside task scope was accepted")
	}
}

func TestRepeaterPolicyAllowsExactURLAllowRule(t *testing.T) {
	u, _ := url.Parse("https://other.test/api")
	p := repeaterPolicy{
		allow: []db.AssetInterceptRule{{Enabled: true, Kind: "exact_url", Pattern: u.String()}},
	}
	if err := p.permits(u, netip.MustParseAddr("203.0.113.10")); err != nil {
		t.Fatalf("exact URL allow rule did not authorize the requested target: %v", err)
	}
	u.Path = "/different"
	if err := p.permits(u, netip.MustParseAddr("203.0.113.10")); err == nil {
		t.Fatal("exact URL allow rule authorized a different URL")
	}
}

func TestResolveRepeaterTargetValidatesEveryDNSAddress(t *testing.T) {
	u, _ := url.Parse("https://example.test/")
	p := repeaterPolicy{scopes: []db.TaskScope{{Kind: "root_domain", Domain: "example.test"}}}
	_, err := resolveRepeaterTarget(context.Background(), u, p, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("169.254.169.254")}, nil
	})
	if err == nil {
		t.Fatal("DNS result containing a reserved address was accepted")
	}
}

func TestSendRepeaterReturnsRedirectWithoutFollowing(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			followed.Store(true)
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer target.Close()

	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	response := sendRepeater(context.Background(), repeaterRequest{Method: http.MethodGet}, u, "1.1", []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if response.Status != http.StatusFound || followed.Load() {
		t.Fatalf("redirect was followed: status=%d followed=%v", response.Status, followed.Load())
	}
}

func TestSendRepeaterCapsResponseBody(t *testing.T) {
	wantBody := strings.Repeat("x", repeaterMaxResponseBody+1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, wantBody)
	}))
	defer target.Close()

	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	response := sendRepeater(context.Background(), repeaterRequest{Method: http.MethodGet}, u, "1.1", []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if !response.BodyTruncated || len(response.Body) != repeaterMaxResponseBody || response.BodyBytes <= repeaterMaxResponseBody {
		t.Fatalf("response limit metadata incorrect: truncated=%v body=%d total=%d", response.BodyTruncated, len(response.Body), response.BodyBytes)
	}
}
