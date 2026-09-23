package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cpa-key-billing/internal/messages"
)

const quotaProxyTimeout = 30 * time.Second
const quotaResponseLimit = 4 << 20

// The v7.2.143 host HTTP callback uses the global proxy, with no auth context.
// Keep an explicit credential proxy local to this call, never on App or in the
// host callback payload (where it would be silently ignored).
type quotaClient struct {
	hostCaller HostCaller
	proxyURL   string
}

func (c quotaClient) do(request hostHTTPRequest) (hostHTTPResponse, error) {
	if c.proxyURL != "" {
		return quotaProxyRequest(request, c.proxyURL)
	}
	raw, err := c.hostCaller(hostHTTPDo, request)
	if err != nil {
		return hostHTTPResponse{}, err
	}
	var response hostHTTPResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return response, messages.Errorf("Parse quota response: %w", err)
	}
	return response, nil
}

func quotaProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "direct") || strings.EqualFold(raw, "none") {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Hostname() != "" {
		switch parsed.Scheme {
		case "http", "https", "socks5", "socks5h":
			return parsed, nil
		}
	}
	// Parse errors may contain proxy usernames/passwords. Never return them.
	return nil, messages.Errorf("Invalid auth file proxy; use HTTP, HTTPS, SOCKS5, SOCKS5H, direct, or none")
}

func quotaProxyRequest(request hostHTTPRequest, proxy string) (hostHTTPResponse, error) {
	proxyURL, err := quotaProxyURL(proxy)
	if err != nil {
		return hostHTTPResponse{}, err
	}
	// One request per transport, like the reference-price downloader. No idle
	// pool or scheduled work survives the host call. net/http handles CONNECT,
	// proxy authentication and SOCKS5 remote DNS; never fall back to direct.
	transport := &http.Transport{
		DisableKeepAlives:   true,
		DialContext:         (&net.Dialer{Timeout: quotaProxyTimeout}).DialContext,
		TLSHandshakeTimeout: quotaProxyTimeout,
	}
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   quotaProxyTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(request.Method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		return hostHTTPResponse{}, messages.Errorf("Invalid upstream response")
	}
	req.Header = request.Headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return hostHTTPResponse{}, messages.Errorf("Auth file proxy request timed out")
		}
		return hostHTTPResponse{}, messages.Errorf("Auth file proxy request failed; check the proxy connection and credentials")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, quotaResponseLimit+1))
	if err != nil {
		return hostHTTPResponse{}, messages.Errorf("Could not read the quota response through the auth file proxy")
	}
	if len(body) > quotaResponseLimit {
		return hostHTTPResponse{}, messages.Errorf("Quota response exceeds the size limit")
	}
	return hostHTTPResponse{StatusCode: resp.StatusCode, Body: body}, nil
}

func (c quotaClient) redact(text, token string) string {
	text = redactSecret(text, token)
	if c.proxyURL == "" {
		return text
	}
	text = redactSecret(text, c.proxyURL)
	if parsed, err := url.Parse(c.proxyURL); err == nil && parsed.User != nil {
		text = redactSecret(text, parsed.User.String())
		text = redactSecret(text, parsed.User.Username())
		if password, ok := parsed.User.Password(); ok {
			text = redactSecret(text, password)
		}
	}
	return text
}
