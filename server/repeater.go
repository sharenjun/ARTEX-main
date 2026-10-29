package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
)

const (
	repeaterMaxRequestBody  = 1 << 20
	repeaterMaxResponseBody = 2 << 20
	repeaterTimeout         = 30 * time.Second
)

type repeaterRequest struct {
	TaskID             string            `json:"task_id"`
	Method             string            `json:"method"`
	URL                string            `json:"url"`
	HTTPVersion        string            `json:"http_version"`
	Headers            map[string]string `json:"headers"`
	Body               string            `json:"body"`
	UseRecordingProxy  bool              `json:"use_recording_proxy"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify"`
}

type repeaterResponse struct {
	Status        int         `json:"status"`
	StatusText    string      `json:"status_text"`
	Proto         string      `json:"proto"`
	Headers       http.Header `json:"headers"`
	Body          string      `json:"body"`
	BodyTruncated bool        `json:"body_truncated"`
	BodyBytes     int         `json:"body_bytes"`
	ElapsedMS     int64       `json:"elapsed_ms"`
	URL           string      `json:"url"`
	Error         string      `json:"error,omitempty"`
}

type repeaterPolicy struct {
	scopes []db.TaskScope
	block  []db.AssetInterceptRule
	allow  []db.AssetInterceptRule
}

// repeaterSend uses its own transport: no ambient proxy, cookie jar, connection
// pool, redirect following, or second DNS lookup can widen the selected task.
func (s *Server) repeaterSend(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 7*repeaterMaxRequestBody)
	var input repeaterRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid repeater request: "+err.Error())
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "request must contain one JSON object")
		return
	}
	input.TaskID = strings.TrimSpace(input.TaskID)
	if input.TaskID == "" {
		writeErr(w, http.StatusBadRequest, "task_id is required")
		return
	}
	if s.m == nil {
		writeErr(w, http.StatusServiceUnavailable, "task store unavailable")
		return
	}
	if s.m.pg == nil {
		writeErr(w, http.StatusServiceUnavailable, "task database unavailable")
		return
	}
	if _, ok := s.m.Task(input.TaskID); !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	u, version, err := validateRepeaterRequest(input)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	taskID, err := strconv.ParseInt(input.TaskID, 10, 64)
	if err != nil || taskID <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid task_id")
		return
	}
	policy, err := s.repeaterPolicy(taskID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "load task authorization: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), repeaterTimeout)
	defer cancel()
	started := time.Now()
	ips, err := resolveRepeaterTarget(ctx, u, policy, net.DefaultResolver.LookupNetIP)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	result := sendRepeater(ctx, input, u, version, ips)
	result.ElapsedMS = time.Since(started).Milliseconds()
	writeJSON(w, http.StatusOK, result)
}

func validateRepeaterRequest(input repeaterRequest) (*url.URL, string, error) {
	if input.UseRecordingProxy {
		return nil, "", fmt.Errorf("recording proxy is unavailable for Repeater; use a direct scoped connection")
	}
	if !httpguts.ValidHeaderFieldName(input.Method) || input.Method == "CONNECT" {
		return nil, "", fmt.Errorf("invalid or unsupported HTTP method")
	}
	if len(input.Body) > repeaterMaxRequestBody {
		return nil, "", fmt.Errorf("request body exceeds 1 MiB")
	}
	u, err := url.Parse(input.URL)
	if err != nil || u == nil || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", fmt.Errorf("url must be an absolute HTTP(S) URL without credentials or fragment")
	}
	if strings.Contains(u.Hostname(), "%") {
		return nil, "", fmt.Errorf("IPv6 zones are unsupported")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, "", fmt.Errorf("invalid URL port")
		}
	}
	version := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(input.HTTPVersion)), "HTTP/")
	if version == "" {
		version = "1.1"
	}
	if version == "2.0" {
		version = "2"
	}
	if version != "1.1" && version != "2" {
		return nil, "", fmt.Errorf("supported HTTP versions are 1.1 and 2")
	}
	if version == "2" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("HTTP/2 requires HTTPS")
	}
	headerBytes := 0
	seen := make(map[string]bool)
	for name, value := range input.Headers {
		key := strings.ToLower(name)
		if seen[key] || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return nil, "", fmt.Errorf("invalid or duplicate HTTP header %q", name)
		}
		seen[key] = true
		headerBytes += len(name) + len(value)
		if headerBytes > 64<<10 {
			return nil, "", fmt.Errorf("request headers exceed 64 KiB")
		}
		switch key {
		case "host":
			if !strings.EqualFold(value, u.Host) {
				return nil, "", fmt.Errorf("Host header must match the URL authority")
			}
		case "content-length":
			if value != strconv.Itoa(len(input.Body)) {
				return nil, "", fmt.Errorf("Content-Length must match the request body size")
			}
		case "transfer-encoding", "proxy-authorization", "proxy-connection":
			return nil, "", fmt.Errorf("header %q is unsupported", name)
		}
	}
	return u, version, nil
}

func (s *Server) repeaterPolicy(taskID int64) (repeaterPolicy, error) {
	var p repeaterPolicy
	as := s.m.Assets()
	if as == nil {
		return p, fmt.Errorf("asset store unavailable")
	}
	var err error
	p.scopes, err = as.ListTaskScopeWithSources(taskID)
	if err != nil {
		return p, err
	}
	// Company scopes authorize their declared domains/networks. Free-text
	// keywords and ICP identifiers cannot authorize a socket on their own.
	for _, scope := range p.scopes {
		if scope.Kind != "company" || scope.CompanyID == nil {
			continue
		}
		rules, err := as.Companies().GetScope(*scope.CompanyID)
		if err != nil {
			return p, err
		}
		for _, rule := range rules {
			kind := rule.Kind
			if kind == "domain" {
				kind = "root_domain"
			}
			p.scopes = append(p.scopes, db.TaskScope{Kind: kind, Domain: rule.Domain, Net: rule.Net})
		}
	}
	p.block, err = as.ListAssetInterceptRules()
	if err != nil {
		return p, err
	}
	block, allow, err := as.TaskInterceptRulesSplit(taskID)
	p.block = append(p.block, block...)
	p.allow = allow
	return p, err
}

func (p repeaterPolicy) permits(u *url.URL, ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.String() == "100.100.100.200" || ip.String() == "168.63.129.16" || ip.String() == "fd00:ec2::254" {
		return fmt.Errorf("target address is reserved or a metadata endpoint")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	domains := []string{host}
	if root, _ := db.RootDomain(host); root != "" && root != host {
		domains = append(domains, root)
	}
	if decision := db.EvaluateAssetGate(p.block, p.allow, domains, []string{ip.String()}, []string{u.String()}); !decision.Allowed {
		return fmt.Errorf("%s", decision.Reason)
	}
	for _, scope := range p.scopes {
		domain := strings.ToLower(strings.TrimSuffix(scope.Domain, "."))
		switch scope.Kind {
		case "root_domain":
			if domain != "" && (host == domain || strings.HasSuffix(host, "."+domain)) {
				return nil
			}
		case "subdomain":
			if domain != "" && host == domain {
				return nil
			}
		case "ip", "cidr":
			if network, err := netip.ParsePrefix(scope.Net); err == nil && network.Contains(ip) {
				return nil
			}
		}
	}
	// An enabled explicit task allow rule is also an authorization edge.
	if _, _, ok := db.MatchAssetInterceptRules(p.allow, domains, []string{ip.String()}, []string{u.String()}); ok {
		return nil
	}
	return fmt.Errorf("target is outside the selected task's explicit domain/IP scope")
}

func resolveRepeaterTarget(ctx context.Context, u *url.URL, policy repeaterPolicy, lookup func(context.Context, string, string) ([]netip.Addr, error)) ([]netip.Addr, error) {
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		ips = []netip.Addr{ip.Unmap()}
	} else {
		var err error
		ips, err = lookup(ctx, "ip", u.Hostname())
		if err != nil {
			return nil, fmt.Errorf("resolve target: %w", err)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("target has no IP addresses")
	}
	for _, ip := range ips {
		if err := policy.permits(u, ip); err != nil {
			return nil, err
		}
	}
	return ips, nil
}

func sendRepeater(ctx context.Context, input repeaterRequest, u *url.URL, version string, ips []netip.Addr) repeaterResponse {
	result := repeaterResponse{URL: u.String(), Headers: make(http.Header)}
	req, err := http.NewRequestWithContext(ctx, input.Method, u.String(), strings.NewReader(input.Body))
	if err != nil {
		result.Error = err.Error()
		return result
	}
	for name, value := range input.Headers {
		if strings.EqualFold(name, "Host") {
			req.Host = value
		} else {
			req.Header.Set(name, value)
		}
	}
	if _, set := req.Header["User-Agent"]; !set {
		req.Header["User-Agent"] = []string{""}
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		var lastErr error
		for _, ip := range ips {
			conn, err := (&net.Dialer{Timeout: repeaterTimeout}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	// The URL hostname remains the TLS identity even though the socket dials an
	// already authorized address. The explicit user toggle controls verification.
	tlsConfig := &tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: input.InsecureSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec
	var transport http.RoundTripper
	var closeIdle func()
	if version == "2" {
		tlsConfig.NextProtos = []string{"h2"}
		tr := &http2.Transport{TLSClientConfig: tlsConfig, DisableCompression: true, MaxHeaderListSize: 1 << 20}
		tr.DialTLSContext = func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			raw, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			conn := tls.Client(raw, cfg)
			if err := conn.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			if conn.ConnectionState().NegotiatedProtocol != "h2" {
				_ = conn.Close()
				return nil, fmt.Errorf("target did not negotiate HTTP/2")
			}
			return conn, nil
		}
		transport, closeIdle = tr, tr.CloseIdleConnections
	} else {
		tlsConfig.NextProtos = []string{"http/1.1"}
		tr := &http.Transport{Proxy: nil, DialContext: dial, TLSClientConfig: tlsConfig,
			TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
			DisableCompression: true, DisableKeepAlives: true, MaxResponseHeaderBytes: 1 << 20,
			TLSHandshakeTimeout: repeaterTimeout, ResponseHeaderTimeout: repeaterTimeout}
		transport, closeIdle = tr, tr.CloseIdleConnections
	}
	defer closeIdle()
	client := &http.Client{Transport: transport, Timeout: repeaterTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer resp.Body.Close()
	result.Status, result.StatusText, result.Proto, result.Headers = resp.StatusCode, resp.Status, resp.Proto, resp.Header.Clone()
	body, err := io.ReadAll(io.LimitReader(resp.Body, repeaterMaxResponseBody+1))
	result.BodyTruncated = len(body) > repeaterMaxResponseBody
	if result.BodyTruncated {
		body = body[:repeaterMaxResponseBody]
	}
	result.Body, result.BodyBytes = string(body), len(body)
	if result.BodyTruncated {
		// The exact size is intentionally not read into memory; expose a lower
		// bound so clients do not mistake the capped body for a complete one.
		result.BodyBytes = repeaterMaxResponseBody + 1
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}
