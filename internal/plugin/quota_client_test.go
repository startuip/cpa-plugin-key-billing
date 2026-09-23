package plugin

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
)

func TestQuotaHTTPProxyUsesAccountCredentials(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "http://quota.invalid/reset" || r.Method != http.MethodPost || !r.Close {
			t.Errorf("unexpected proxy request: %s %s close=%v", r.Method, r.URL, r.Close)
		}
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("dummy-user:dummy-password")) || r.Header.Get("Authorization") != "Bearer dummy-token" {
			t.Error("missing separate proxy and upstream authentication")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"redeem_request_id":"dummy-operation"}` {
			t.Errorf("body = %s", body)
		}
		_, _ = io.WriteString(w, `{"reset":true}`)
	}))
	defer proxy.Close()
	client := quotaClient{proxyURL: strings.Replace(proxy.URL, "://", "://dummy-user:dummy-password@", 1), hostCaller: func(string, any) (json.RawMessage, error) {
		t.Fatal("credential proxy fell back to the host/global proxy")
		return nil, nil
	}}
	result, err := client.upstream("", http.MethodPost, "http://quota.invalid/reset", "dummy-token", nil, map[string]string{"redeem_request_id": "dummy-operation"})
	if err != nil || result["reset"] != true {
		t.Fatalf("result = %v, error = %v", result, err)
	}
}

func TestQuotaSOCKSProxyUsesRemoteDNSAndAuthentication(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- serveQuotaSOCKS(listener) }()
			defer func() {
				_ = listener.Close()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			client := quotaClient{proxyURL: scheme + "://dummy-user:dummy-password@" + listener.Addr().String()}
			result, err := client.upstream("", http.MethodGet, "http://quota.invalid/usage", "dummy-token", nil, nil)
			if err != nil || result["ok"] != true {
				t.Fatalf("result = %v, error = %v", result, err)
			}
		})
	}
}

// A minimal one-connection SOCKS server. The reserved .invalid name must reach
// the proxy as a domain, and cannot succeed through a direct DNS lookup.
func serveQuotaSOCKS(listener net.Listener) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	read := func(n int) ([]byte, error) {
		b := make([]byte, n)
		_, err := io.ReadFull(reader, b)
		return b, err
	}
	header, err := read(2)
	if err != nil || header[0] != 5 {
		return fmt.Errorf("invalid SOCKS greeting: %v", err)
	}
	if _, err = read(int(header[1])); err != nil {
		return err
	}
	_, _ = conn.Write([]byte{5, 2})
	userHeader, err := read(2)
	if err != nil {
		return err
	}
	user, err := read(int(userHeader[1]))
	if err != nil {
		return err
	}
	length, err := reader.ReadByte()
	if err != nil {
		return err
	}
	password, err := read(int(length))
	if err != nil || string(user) != "dummy-user" || string(password) != "dummy-password" {
		return fmt.Errorf("invalid SOCKS authentication")
	}
	_, _ = conn.Write([]byte{1, 0})
	header, err = read(5)
	if err != nil || header[0] != 5 || header[1] != 1 || header[3] != 3 {
		return fmt.Errorf("expected SOCKS domain CONNECT: %v", err)
	}
	domain, err := read(int(header[4]))
	if err != nil || string(domain) != "quota.invalid" {
		return fmt.Errorf("remote DNS target was not preserved")
	}
	if _, err = read(2); err != nil {
		return err
	}
	_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
	req, err := http.ReadRequest(reader)
	if err != nil {
		return err
	}
	defer req.Body.Close()
	if req.Header.Get("Authorization") != "Bearer dummy-token" || req.Header.Get("Proxy-Authorization") != "" {
		return fmt.Errorf("upstream/proxy credentials were mixed")
	}
	_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 11\r\nConnection: close\r\n\r\n{\"ok\":true}")
	return err
}

func TestQuotaExplicitDirectBypassesEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, setting := range []string{"direct", "NONE"} {
		t.Run(setting, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer server.Close()
			result, err := (quotaClient{proxyURL: setting}).upstream("", http.MethodGet, server.URL, "dummy-token", nil, nil)
			if err != nil || result["ok"] != true {
				t.Fatalf("result = %v, error = %v", result, err)
			}
		})
	}
}

func TestQuotaProxyFailuresNeverFallbackOrExposeSecrets(t *testing.T) {
	var direct atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1) }))
	defer origin.Close()
	for _, proxy := range []string{"ftp://dummy-user:dummy-password@proxy.invalid", "http://dummy-user:dummy-password@%zz", "http://dummy-user:dummy-password@127.0.0.1:1"} {
		_, err := (quotaClient{proxyURL: proxy}).upstream("", http.MethodGet, origin.URL, "dummy-token", nil, nil)
		if err == nil || messages.FromError(err).Key == "" {
			t.Fatalf("missing translated proxy failure: %v", err)
		}
		if strings.Contains(err.Error(), "dummy-") {
			t.Fatalf("proxy failure leaked credentials: %v", err)
		}
	}
	if direct.Load() != 0 {
		t.Fatal("failed credential proxy fell back to a direct request")
	}
}

func TestQuotaProxyRejectsRedirectsLargeBodiesAndUntrustedTLS(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large" {
			_, _ = io.WriteString(w, strings.Repeat("x", quotaResponseLimit+1))
			return
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer proxy.Close()
	for _, path := range []string{"redirect", "large"} {
		_, err := (quotaClient{proxyURL: proxy.URL}).upstream("", http.MethodGet, "http://quota.invalid/"+path, "dummy-token", nil, nil)
		if err == nil {
			t.Fatalf("accepted %s response", path)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("followed an upstream redirect")
	}
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("accepted an untrusted proxy certificate") }))
	defer untrusted.Close()
	if _, err := (quotaClient{proxyURL: untrusted.URL}).upstream("", http.MethodGet, "https://quota.invalid/usage", "dummy-token", nil, nil); err == nil {
		t.Fatal("TLS verification was disabled")
	}
}

func TestQuotaProxyUpstreamErrorRedactsBothCredentials(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"dummy-token dummy-user dummy-password"}`)
	}))
	defer proxy.Close()
	client := quotaClient{proxyURL: strings.Replace(proxy.URL, "://", "://dummy-user:dummy-password@", 1)}
	_, err := client.upstream("", http.MethodGet, "http://quota.invalid/usage", "dummy-token", nil, nil)
	if err == nil || strings.Contains(err.Error(), "dummy-") || strings.Contains(fmt.Sprint(messages.FromError(err).Params), "dummy-") {
		t.Fatalf("leaked error = %v", err)
	}
}

func TestResetFollowProxyFailurePreservesConfigurationAndUsage(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	scope := billing.CallerScope("sk-dummy-follow-a")
	now := app.store.Now()
	app.store.RecordUsage(billing.UsageEvent{Scope: scope, UpstreamModel: "gpt-5.5", At: now, RequestedAt: now})
	before, _ := app.store.KeyViewForScope(scope)
	app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case hostAuthList:
			return json.Marshal(hostAuthListResponse{Files: []hostAuthFile{file}})
		case hostAuthGet:
			return json.RawMessage(`{"json":{"access_token":"dummy-token","proxy_url":"ftp://dummy-user:dummy-password@proxy.invalid"}}`), nil
		default:
			t.Fatalf("unexpected host call after explicit proxy: %s", method)
			return nil, nil
		}
	})
	response := app.applyResetFollow(ManagementRequest{}, scope, file.AuthIndex)
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(response.Body), "backend.invalid_auth_file_proxy") {
		t.Fatalf("response = %d %s", response.StatusCode, response.Body)
	}
	after, _ := app.store.KeyViewForScope(scope)
	if after.ResetFollow.Error.Key != "backend.invalid_auth_file_proxy" {
		t.Fatalf("follow state lost the proxy error translation: %+v", after.ResetFollow.Error)
	}
	if after.ResetFollow.AuthIndex != before.ResetFollow.AuthIndex || after.Windows[0].Dimensions[0].Used != before.Windows[0].Dimensions[0].Used {
		t.Fatal("failed proxy query replaced follow configuration or cleared usage")
	}
}
