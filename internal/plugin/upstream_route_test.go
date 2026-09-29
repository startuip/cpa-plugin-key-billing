package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestAuthUpstreamRouteFollowsCPAProxySemantics(t *testing.T) {
	for _, test := range []struct {
		proxy       string
		direct      bool
		proxyScheme string
		invalid     bool
	}{
		{proxy: ""},
		{proxy: "direct", direct: true},
		{proxy: " NONE ", direct: true},
		{proxy: "http://127.0.0.1:8080", direct: true, proxyScheme: "http"},
		{proxy: "https://proxy.example:443", direct: true, proxyScheme: "https"},
		{proxy: "socks5://user:pass@127.0.0.1:1080", direct: true, proxyScheme: "socks5"},
		{proxy: "SOCKS5H://127.0.0.1:1080", direct: true, proxyScheme: "socks5h"},
		{proxy: "ftp://user:proxy-secret@127.0.0.1:21", invalid: true},
		{proxy: "http://", invalid: true},
		{proxy: "127.0.0.1:1080", invalid: true},
	} {
		route, err := authUpstreamRoute("callback", map[string]any{"metadata": map[string]any{"proxy_url": test.proxy}})
		if test.invalid {
			if err == nil || strings.Contains(err.Error(), "proxy-secret") {
				t.Fatalf("%q: error = %v", test.proxy, err)
			}
			continue
		}
		if err != nil || route.callbackID != "callback" || route.direct != test.direct ||
			(route.proxy == nil) != (test.proxyScheme == "") || route.proxy != nil && route.proxy.Scheme != test.proxyScheme {
			t.Fatalf("%q: route = %+v, err = %v", test.proxy, route, err)
		}
	}
}

func hostCallerWithoutHTTP(t *testing.T, auth string) HostCaller {
	return func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case hostAuthGet:
			return json.RawMessage(auth), nil
		case hostHTTPDo:
			t.Error("an auth file with its own proxy must not be requested through the host")
		default:
			t.Errorf("unexpected host method %q", method)
		}
		return nil, fmt.Errorf("unexpected host method %q", method)
	}
}

func TestAuthProxyCarriesQuotaRequests(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.RequestURI+" "+r.Header.Get("Authorization")+" "+r.Header.Get("Proxy-Authorization"))
		mu.Unlock()
		if r.Method == http.MethodConnect {
			http.Error(w, "tunnels refused", http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"proxied":true,"body":%q}`, body)
	}))
	defer proxy.Close()
	proxyURL := strings.Replace(proxy.URL, "http://", "http://quota-user:proxy-secret@", 1)

	route, err := authUpstreamRoute("", map[string]any{"proxy_url": proxyURL})
	if err != nil {
		t.Fatal(err)
	}
	app := newConfiguredApp(t)
	app.SetHostCaller(hostCallerWithoutHTTP(t, `{}`))
	object, err := app.upstream(route, http.MethodPost, "http://quota.example.test/usage", "dummy-token", nil, map[string]string{"x": "y"})
	if err != nil || object["proxied"] != true || object["body"] != `{"x":"y"}` {
		t.Fatalf("object = %v, err = %v", object, err)
	}

	// HTTPS quota endpoints are tunneled through the same proxy.
	app.SetHostCaller(hostCallerWithoutHTTP(t, fmt.Sprintf(`{"json":{"access_token":"dummy-token","account_id":"acct","proxy_url":%q}}`, proxyURL)))
	_, err = app.fetchAuthQuota("callback", hostAuthFile{AuthIndex: "codex-1"}, "codex")
	if err == nil || strings.Contains(err.Error(), "proxy-secret") || strings.Contains(err.Error(), "dummy-token") {
		t.Fatalf("error = %v, want a refused tunnel without secrets", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || !strings.HasPrefix(seen[0], "POST http://quota.example.test/usage Bearer dummy-token Basic ") ||
		!strings.HasPrefix(seen[1], "CONNECT chatgpt.com:443  Basic ") {
		t.Fatalf("proxy saw %q", seen)
	}
}

func TestDirectAuthProxyBypassesTheHost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dummy-token" {
			http.Error(w, `{"error":{"message":"missing token"}}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"direct":true}`)
	}))
	defer upstream.Close()
	app := newConfiguredApp(t)
	app.SetHostCaller(hostCallerWithoutHTTP(t, `{}`))
	route, err := authUpstreamRoute("", map[string]any{"proxy_url": "direct"})
	if err != nil {
		t.Fatal(err)
	}
	if object, err := app.upstream(route, http.MethodGet, upstream.URL, "dummy-token", nil, nil); err != nil || object["direct"] != true {
		t.Fatalf("object = %v, err = %v", object, err)
	}
	if _, err := app.upstream(route, http.MethodGet, upstream.URL, "wrong-token", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "Credentials are invalid or expired: missing token") {
		t.Fatalf("error = %v", err)
	}
}

func TestUnreachableAuthProxyFailsWithoutSecrets(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	route, err := authUpstreamRoute("", map[string]any{"proxy_url": "socks5://quota-user:proxy-secret@" + address})
	if err != nil {
		t.Fatal(err)
	}
	app := newConfiguredApp(t)
	app.SetHostCaller(hostCallerWithoutHTTP(t, `{}`))
	_, err = app.upstream(route, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", "dummy-token", nil, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "Quota request failed:") || strings.Contains(err.Error(), "proxy-secret") ||
		strings.Contains(err.Error(), "wham/usage") {
		t.Fatalf("error = %v", err)
	}
}
