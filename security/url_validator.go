// Package security provides security utilities for the application
package security

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Private/Reserved IP ranges that should be blocked to prevent SSRF
var privateIPBlocks []*net.IPNet

func init() {
	// Initialize private IP blocks
	// These ranges should not be accessible via user-controlled URLs
	privateRanges := []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC1918 private
		"172.16.0.0/12",  // RFC1918 private
		"192.168.0.0/16", // RFC1918 private
		"169.254.0.0/16", // Link-local / Cloud metadata
		"0.0.0.0/8",      // Current network
		"224.0.0.0/4",    // Multicast
		"240.0.0.0/4",    // Reserved
		"::1/128",        // IPv6 loopback
		"fe80::/10",      // IPv6 link-local
		"fc00::/7",       // IPv6 unique local
	}

	for _, cidr := range privateRanges {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil {
			privateIPBlocks = append(privateIPBlocks, block)
		}
	}
}

// SSRFError represents a Server-Side Request Forgery attempt
type SSRFError struct {
	URL     string
	Reason  string
}

func (e *SSRFError) Error() string {
	return fmt.Sprintf("SSRF blocked: %s - %s", e.URL, e.Reason)
}

// isPrivateIP checks if an IP address is in a private/reserved range
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true // Invalid IP, treat as private
	}

	// Check if it's a loopback address
	if ip.IsLoopback() {
		return true
	}

	// Check if it's a link-local address
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	// Check if it's a private address
	if ip.IsPrivate() {
		return true
	}

	// Check against our explicit private ranges
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}

	return false
}

// ValidateURL checks if a URL is safe to request (not pointing to internal networks)
// Returns an error if the URL is potentially dangerous
func ValidateURL(rawURL string) error {
	if rawURL == "" {
		return &SSRFError{URL: rawURL, Reason: "empty URL"}
	}

	// Parse the URL
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return &SSRFError{URL: rawURL, Reason: "invalid URL format"}
	}

	// Only allow http and https schemes
	scheme := strings.ToLower(parsedURL.Scheme)
	if scheme != "http" && scheme != "https" {
		return &SSRFError{URL: rawURL, Reason: fmt.Sprintf("unsupported scheme: %s", scheme)}
	}

	// Extract hostname (without port)
	host := parsedURL.Hostname()
	if host == "" {
		return &SSRFError{URL: rawURL, Reason: "empty hostname"}
	}

	// Block localhost and common internal hostnames
	lowerHost := strings.ToLower(host)
	blockedHosts := []string{
		"localhost",
		"127.0.0.1",
		"::1",
		"0.0.0.0",
		"metadata.google.internal",
		"metadata.google",
		"instance-data",
	}
	for _, blocked := range blockedHosts {
		if lowerHost == blocked {
			return &SSRFError{URL: rawURL, Reason: fmt.Sprintf("blocked hostname: %s", host)}
		}
	}

	// Resolve the hostname to IP addresses
	// This catches DNS rebinding and ensures we check the actual destination
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resolver := net.Resolver{}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		// If DNS resolution fails, we still need to check if it's an IP address directly
		ip := net.ParseIP(host)
		if ip != nil {
			if isPrivateIP(ip) {
				return &SSRFError{URL: rawURL, Reason: "resolves to private IP address"}
			}
			return nil // It's a valid public IP
		}
		// DNS resolution failed, but it's not an IP - could be a typo or non-existent domain
		// Allow it and let the HTTP client handle the error
		return nil
	}

	// Check all resolved IPs
	for _, ipAddr := range ips {
		if isPrivateIP(ipAddr.IP) {
			return &SSRFError{URL: rawURL, Reason: fmt.Sprintf("resolves to private IP: %s", ipAddr.IP)}
		}
	}

	return nil
}

// ProxyURL returns the outbound proxy configured via the PROXY_URL environment
// variable (e.g. http://127.0.0.1:7897), or nil when unset or invalid.
func ProxyURL() *url.URL {
	v := strings.TrimSpace(os.Getenv("PROXY_URL"))
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return u
}

// ProxyFunc returns a transport proxy function honouring NO_PROXY: a
// comma-separated list of hostnames that must bypass PROXY_URL (standard
// semantics: "bigmodel.cn" matches the host and its subdomains, ".cn" matches
// suffixes, "*" bypasses everything). Returns nil when no proxy is configured.
func ProxyFunc() func(*http.Request) (*url.URL, error) {
	proxy := ProxyURL()
	if proxy == nil {
		return nil
	}
	var rules []string
	for _, entry := range strings.Split(os.Getenv("NO_PROXY"), ",") {
		if entry = strings.ToLower(strings.TrimSpace(entry)); entry != "" {
			rules = append(rules, entry)
		}
	}
	if len(rules) == 0 {
		return http.ProxyURL(proxy)
	}
	return func(req *http.Request) (*url.URL, error) {
		if hostBypassesProxy(req.URL.Hostname(), rules) {
			return nil, nil
		}
		return proxy, nil
	}
}

func hostBypassesProxy(host string, rules []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, rule := range rules {
		if rule == "*" {
			return true
		}
		rule = strings.TrimPrefix(rule, ".")
		if host == rule || strings.HasSuffix(host, "."+rule) {
			return true
		}
	}
	return false
}

// SafeHTTPClient returns an HTTP client with SSRF protection
// It validates URLs and blocks requests to private networks.
// When PROXY_URL is set, requests are dialed through the proxy instead; the
// hostname then resolves at the proxy, so the direct-dial IP check is skipped.
// Hosts listed in NO_PROXY bypass the proxy (and go through the SSRF check).
func SafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{}
	if proxy := ProxyURL(); proxy != nil {
		transport.Proxy = ProxyFunc()
		transport.DialContext = dialer.DialContext
	} else {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Extract host from address
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}

			// Resolve and check the IP
			ips, err := net.LookupIP(host)
			if err != nil {
				return nil, fmt.Errorf("SSRF protection: failed to resolve host %s: %w", host, err)
			}

			for _, ip := range ips {
				if isPrivateIP(ip) {
					return nil, fmt.Errorf("SSRF protection: blocked connection to private IP %s", ip)
				}
			}

			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}

			// Validate the redirect URL
			if err := ValidateURL(req.URL.String()); err != nil {
				return fmt.Errorf("SSRF protection: redirect blocked - %w", err)
			}

			return nil
		},
	}
}

// SafeGet performs a GET request with SSRF protection
// It validates the URL before making the request and uses a safe HTTP client
func SafeGet(rawURL string, timeout time.Duration) (*http.Response, error) {
	// First validate the URL
	if err := ValidateURL(rawURL); err != nil {
		return nil, err
	}

	// Use the safe HTTP client
	client := SafeHTTPClient(timeout)
	return client.Get(rawURL)
}
