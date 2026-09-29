package plugin

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"cpa-key-billing/internal/messages"
)

const (
	directQuotaTimeout          = 20 * time.Second
	maxDirectQuotaResponseBytes = 4 << 20
)

// upstreamRoute is how quota requests for one auth file leave the process.
// host.http.do applies only CPA's global proxy, so an auth file with its own
// proxy_url is requested by the plugin through that proxy, as CPA would send
// its model requests. The request completes within the host call and keeps no
// idle connection, so nothing outlives the call.
type upstreamRoute struct {
	callbackID string
	direct     bool
	proxy      *url.URL // nil for "direct" or "none"
}

// authUpstreamRoute follows CPA's proxy_url semantics: empty inherits the
// global proxy, "direct" and "none" bypass proxies, and otherwise an http,
// https, socks5, or socks5h URL is required.
func authUpstreamRoute(callbackID string, credential map[string]any) (upstreamRoute, error) {
	route := upstreamRoute{callbackID: callbackID}
	raw := credentialString(credential, "proxy_url", "proxyUrl")
	if raw == "" {
		return route, nil
	}
	route.direct = true
	if strings.EqualFold(raw, "direct") || strings.EqualFold(raw, "none") {
		return route, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || !slices.Contains([]string{"http", "https", "socks5", "socks5h"}, parsed.Scheme) {
		return upstreamRoute{}, messages.Errorf("The auth file proxy URL is invalid")
	}
	route.proxy = parsed
	return route, nil
}

func (r upstreamRoute) send(method, endpoint string, headers http.Header, body []byte, secret string) (hostHTTPResponse, error) {
	transport := &http.Transport{DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second}
	if r.proxy != nil {
		transport.Proxy = http.ProxyURL(r.proxy)
	}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		return hostHTTPResponse{}, messages.Errorf("Build quota request: %w", err)
	}
	request.Header = headers.Clone()
	response, err := (&http.Client{Transport: transport, Timeout: directQuotaTimeout}).Do(request)
	if err != nil {
		// Report the cause without the request URL, and never the proxy credentials.
		var requestError *url.Error
		for errors.As(err, &requestError) {
			err = requestError.Err
		}
		return hostHTTPResponse{}, messages.Errorf("Quota request failed: %s", r.redact(err.Error(), secret))
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxDirectQuotaResponseBytes+1))
	if err != nil {
		return hostHTTPResponse{}, messages.Errorf("Quota request failed: %s", r.redact(err.Error(), secret))
	}
	if len(raw) > maxDirectQuotaResponseBytes {
		return hostHTTPResponse{}, messages.Errorf("The quota response exceeds the size limit")
	}
	return hostHTTPResponse{StatusCode: response.StatusCode, Body: raw}, nil
}

func (r upstreamRoute) redact(value, secret string) string {
	value = redactSecret(value, secret)
	if r.proxy != nil && r.proxy.User != nil {
		if password, ok := r.proxy.User.Password(); ok {
			value = redactSecret(value, password)
		}
	}
	return value
}
