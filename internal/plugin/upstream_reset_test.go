package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func TestCodexUpstreamObservations(t *testing.T) {
	observedAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	var info map[string]any
	if err := json.Unmarshal([]byte(`{
		"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1},
		"secondary_window":{"usedPercent":"40","limitWindowSeconds":604800,"resetAt":1790000000},
		"tertiary_window":{"used_percent":1,"limit_window_seconds":60,"reset_after_seconds":1}
	}`), &info); err != nil {
		t.Fatal(err)
	}
	got := codexUpstreamObservations(info, observedAt)
	want := []billing.UpstreamObservation{
		{PeriodSeconds: 18000, ResetAt: observedAt.Add(10 * time.Minute), UsedPercent: 12},
		{PeriodSeconds: 604800, ResetAt: time.Unix(1790000000, 0).UTC(), UsedPercent: 40},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("observations = %+v, want %+v", got, want)
	}
	for _, invalid := range []string{
		`{"primary_window":{"limit_window_seconds":18000,"reset_after_seconds":600}}`,
		`{"primary_window":{"used_percent":1,"reset_after_seconds":600}}`,
		`{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}`,
	} {
		info = nil
		if err := json.Unmarshal([]byte(invalid), &info); err != nil {
			t.Fatal(err)
		}
		if got := codexUpstreamObservations(info, observedAt); len(got) != 0 {
			t.Fatalf("%s produced %+v", invalid, got)
		}
	}
}

type fakeCodexUpstream struct {
	mu        sync.Mutex
	usage     string
	status    int
	calls     int
	callbacks []string
}

func (f *fakeCodexUpstream) set(status int, usage string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.usage = status, usage
}

func (f *fakeCodexUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCodexUpstream) host(t *testing.T) HostCaller {
	return func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case hostAuthList:
			return json.RawMessage(`{"files":[
				{"id":"codex-user.json","auth_index":"c1","name":"codex-user.json","type":"codex","email":"user@example.com","path":"/auths/codex-user.json"},
				{"id":"codex-off.json","auth_index":"c2","name":"codex-off.json","type":"codex","disabled":true},
				{"id":"claude.json","auth_index":"a1","name":"claude.json","type":"claude"}
			]}`), nil
		case hostAuthGet:
			return json.RawMessage(`{"json":{"access_token":"dummy-upstream-token","account_id":"dummy-account"}}`), nil
		case hostHTTPDo:
			request := payload.(hostHTTPRequest)
			if request.URL != "https://chatgpt.com/backend-api/wham/usage" || request.Headers.Get("Chatgpt-Account-Id") != "dummy-account" {
				t.Errorf("unexpected upstream request: %+v", request)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls++
			f.callbacks = append(f.callbacks, request.HostCallbackID)
			return mustJSONRaw(t, hostHTTPResponse{StatusCode: f.status, Body: []byte(f.usage)}), nil
		default:
			t.Errorf("unexpected host method %q", method)
			return nil, fmt.Errorf("unexpected host method %q", method)
		}
	}
}

func codexWeeklyUsage(usedPercent float64, resetAfter time.Duration) string {
	return fmt.Sprintf(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":%g,"limit_window_seconds":604800,"reset_after_seconds":%d}}}`,
		usedPercent, int64(resetAfter/time.Second))
}

func interceptWithCallback(t *testing.T, app *App, callbackID string) RequestInterceptResponse {
	t.Helper()
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
		SourceFormat: "openai", Model: "gpt-5.5", HostCallbackID: callbackID,
		Metadata: map[string]any{MetadataCallerScope: billing.CallerScope(testAPIKey)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response RequestInterceptResponse
	decodeResult(t, raw, &response)
	return response
}

func followingPlan(t *testing.T, app *App, credential string) ManagementResponse {
	t.Helper()
	return app.routeManagement(ManagementRequest{
		Method: http.MethodPatch, Path: managementBase + routePlans,
		Body: mustMarshal(t, map[string]any{"id": "plan-5", "upstream_reset": map[string]string{"credential": credential}}),
	}, routePlans)
}

func TestExhaustedKeyFollowsEarlyCodexReset(t *testing.T) {
	app := exhaustedApp(t, 7*24*time.Hour)
	upstream := &fakeCodexUpstream{}
	app.SetHostCaller(upstream.host(t))
	week := 7 * 24 * time.Hour

	for credential, wantStatus := range map[string]int{
		billing.CredentialFingerprint("missing.json"):   http.StatusBadRequest,
		billing.CredentialFingerprint("codex-off.json"): http.StatusBadRequest,
		billing.CredentialFingerprint("claude.json"):    http.StatusBadRequest,
		"sha256:invalid": http.StatusBadRequest,
	} {
		if response := followingPlan(t, app, credential); response.StatusCode != wantStatus {
			t.Fatalf("following %s: status = %d, body = %s", credential, response.StatusCode, response.Body)
		}
	}
	credential := billing.CredentialFingerprint("codex-user.json")
	if response := followingPlan(t, app, credential); response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
	}

	// The first check records the upstream window as the baseline.
	upstream.set(http.StatusOK, codexWeeklyUsage(97, 3*24*time.Hour))
	if response := interceptWithCallback(t, app, "callback-1"); !response.Terminate {
		t.Fatalf("response = %+v, want the exhausted key rejected", response)
	}
	if response := interceptWithCallback(t, app, "callback-2"); !response.Terminate || upstream.callCount() != 1 {
		t.Fatalf("response = %+v, calls = %d, want no second check within the interval", response, upstream.callCount())
	}

	// A new window that started before the previous end is an early reset.
	upstream.set(http.StatusOK, codexWeeklyUsage(1, week-3*time.Minute))
	app.upstreamChecks.states[credential].attemptedAt = app.store.Now().Add(-upstreamResetCheckInterval)
	if response := interceptWithCallback(t, app, "callback-3"); response.Terminate {
		t.Fatalf("response = %s, want the key admitted after the upstream reset", response.ResponseBody)
	}
	if upstream.callCount() != 2 || strings.Join(upstream.callbacks, ",") != "callback-1,callback-3" {
		t.Fatalf("callbacks = %v", upstream.callbacks)
	}

	var plans []planRow
	response := app.routeManagement(ManagementRequest{Method: http.MethodGet, Path: managementBase + routePlans}, routePlans)
	if err := json.Unmarshal(response.Body, &struct{ Plans *[]planRow }{&plans}); err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].UpstreamStatus == nil || plans[0].UpstreamStatus.CheckedAt.IsZero() || plans[0].UpstreamStatus.Error != "" ||
		plans[0].UpstreamReset.LastResetAt.IsZero() || upstream.callCount() != 2 {
		t.Fatalf("plans = %s, calls = %d", response.Body, upstream.callCount())
	}
	for _, forbidden := range []string{"dummy-upstream-token", "codex-user.json", "user@example.com"} {
		if strings.Contains(string(response.Body), forbidden) {
			t.Fatalf("plans leaked %q: %s", forbidden, response.Body)
		}
	}
}

func TestUpstreamResetCheckFailureIsReportedOnce(t *testing.T) {
	app := exhaustedApp(t, 7*24*time.Hour)
	upstream := &fakeCodexUpstream{}
	app.SetHostCaller(upstream.host(t))
	credential := billing.CredentialFingerprint("codex-user.json")
	if response := followingPlan(t, app, credential); response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
	}
	upstream.set(http.StatusUnauthorized, `{"error":{"message":"token expired"}}`)
	for range 2 {
		response := app.routeManagement(ManagementRequest{
			Method: http.MethodPost, Path: managementBase + routePlansUpstreamSync, Body: mustMarshal(t, map[string]string{"id": "plan-5"}),
		}, routePlansUpstreamSync)
		if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), "Credentials are invalid or expired") {
			t.Fatalf("sync = %d %s", response.StatusCode, response.Body)
		}
	}
	if upstream.callCount() != 2 {
		t.Fatalf("calls = %d, want a forced check each time", upstream.callCount())
	}
	logs, err := app.store.PluginLogsPage(billing.PluginLogQuery{Limit: 20, Levels: []billing.PluginLogLevel{billing.PluginLogError}})
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, entry := range logs.Entries {
		if strings.Contains(entry.Message, "Failed to check the followed upstream auth file") {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("logs = %+v, want the onset reported once", logs.Entries)
	}
	if response := interceptWithCallback(t, app, ""); !response.Terminate {
		t.Fatal("a failed check must not change the quota decision")
	}
	missing := app.routeManagement(ManagementRequest{
		Method: http.MethodPost, Path: managementBase + routePlansUpstreamSync, Body: mustMarshal(t, map[string]string{"id": "missing"}),
	}, routePlansUpstreamSync)
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing plan sync = %d %s", missing.StatusCode, missing.Body)
	}
}
