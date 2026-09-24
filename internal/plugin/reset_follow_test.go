package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
)

func resetFollowApp(t *testing.T) (*App, string, hostAuthFile, string) {
	t.Helper()
	app, path := newAppWithPriceAndState(t, true)
	keys := []string{"sk-dummy-follow-a", "sk-dummy-follow-b"}
	if _, err := app.store.SyncKeys(keys, false); err != nil {
		t.Fatal(err)
	}
	plan, err := app.store.CreatePlanWithBindings(billing.Plan{Name: "Follow", Windows: []billing.QuotaWindow{{Name: "5 hours", PeriodSeconds: 18000, RequestLimit: 3}}}, []string{billing.CallerScope(keys[0]), billing.CallerScope(keys[1])})
	if err != nil {
		t.Fatal(err)
	}
	file := hostAuthFile{ID: "dummy-host-id", AuthIndex: "dummy-follow-index", Type: "codex", Provider: "codex", Name: "dummy-codex.json"}
	now := app.store.Now()
	app.store.ApplyResetSnapshot(billing.ResetSnapshot{AuthIndex: file.AuthIndex, Provider: "codex", CredentialRef: billing.CredentialFingerprint(file.ID), AttemptedAt: now, SyncedAt: now,
		Windows: []billing.UpstreamWindow{{ID: "primary_window", PeriodSeconds: 18000, ResetAt: now.Add(time.Hour)}}})
	for _, key := range keys {
		if err := app.store.SetResetFollow(billing.CallerScope(key), file.AuthIndex); err != nil {
			t.Fatal(err)
		}
	}
	return app, path, file, plan.Windows[0].ID
}

func resetFollowHost(t *testing.T, file hostAuthFile, query func(hostHTTPRequest) (json.RawMessage, error)) HostCaller {
	t.Helper()
	return func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case hostAuthList:
			return json.Marshal(hostAuthListResponse{Files: []hostAuthFile{file}})
		case hostAuthGet:
			return json.RawMessage(`{"json":{"access_token":"dummy-token"}}`), nil
		case hostHTTPDo:
			return query(payload.(hostHTTPRequest))
		default:
			return nil, fmt.Errorf("unexpected method %s", method)
		}
	}
}

func resetQuotaResponse() json.RawMessage {
	raw, _ := json.Marshal(hostHTTPResponse{StatusCode: 200, Body: []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":99,"reset_at":%d}}}`, time.Now().Add(2*time.Hour).Unix()))})
	return raw
}

func waitFollowSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for reset synchronization")
	}
}

func followManagementCall(t *testing.T, app *App, req ManagementRequest) ManagementResponse {
	t.Helper()
	raw, err := app.HandleMethod(MethodManagementHandle, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var response ManagementResponse
	decodeResult(t, raw, &response)
	return response
}

func TestResetFollowExplicitRefreshDeduplicatesAndPreservesUsage(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	var queries atomic.Int32
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		if req.HostCallbackID != "page-callback" {
			t.Errorf("refresh lost the host request context: %q", req.HostCallbackID)
		}
		if strings.HasSuffix(req.URL, "/usage") {
			queries.Add(1)
		}
		return resetQuotaResponse(), nil
	}))
	scope := billing.CallerScope("sk-dummy-follow-a")
	now := app.store.Now()
	app.store.RecordUsage(billing.UsageEvent{Scope: scope, UpstreamModel: "gpt-5.5", At: now, RequestedAt: now})
	req := ManagementRequest{Method: http.MethodGet, Path: managementBase + routeKeys, HostCallbackID: "page-callback"}
	if response := followManagementCall(t, app, req); response.StatusCode != 200 || queries.Load() != 0 {
		t.Fatalf("ordinary quota read contacted upstream: %s", response.Body)
	}
	req.Query = map[string][]string{"refresh_reset_follow": {"1"}}
	response := followManagementCall(t, app, req)
	if response.StatusCode != 200 || queries.Load() != 1 {
		t.Fatalf("two followers must share one query per page refresh: %s, queries=%d", response.Body, queries.Load())
	}
	view, _ := app.store.KeyViewForScope(scope)
	if view.Windows[0].Dimensions[0].Used.String() != "1" {
		t.Fatal("refresh cleared usage")
	}
	response = followManagementCall(t, app, req)
	if response.StatusCode != 200 || queries.Load() != 2 {
		t.Fatal("next explicit page refresh did not synchronize")
	}
}

func TestResetFollowLifecycleWaitsForForegroundRefresh(t *testing.T) {
	for _, lifecycle := range []string{MethodPluginQuiesce, MethodPluginReconfigure, "shutdown"} {
		t.Run(lifecycle, func(t *testing.T) {
			app, path, file, _ := resetFollowApp(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var queries atomic.Int32
			app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
				if strings.HasSuffix(req.URL, "/usage") {
					queries.Add(1)
					close(entered)
					<-release
				}
				return resetQuotaResponse(), nil
			}))
			request := mustMarshal(t, ManagementRequest{Method: http.MethodGet, Path: managementBase + routeKeys,
				Query: map[string][]string{"refresh_reset_follow": {"1"}}, HostCallbackID: "foreground"})
			refreshed := make(chan struct{})
			go func() { defer close(refreshed); _, _ = app.HandleMethod(MethodManagementHandle, request) }()
			waitFollowSignal(t, entered)
			finished := make(chan struct{})
			config := mustMarshal(t, LifecycleRequest{ConfigYAML: []byte(fmt.Sprintf("enabled: false\nstate_file: %q\n", path))})
			go func() {
				defer close(finished)
				if lifecycle == "shutdown" {
					app.Shutdown()
				} else if _, err := app.HandleMethod(lifecycle, config); err != nil {
					t.Error(err)
				}
			}()
			select {
			case <-finished:
				close(release)
				t.Fatal("lifecycle returned before the in-flight host call")
			case <-time.After(25 * time.Millisecond):
			}
			close(release)
			waitFollowSignal(t, refreshed)
			waitFollowSignal(t, finished)
			// Disabled, quiesced, and closed instances do not issue new queries.
			_, _ = app.HandleMethod(MethodManagementHandle, request)
			if queries.Load() != 1 {
				t.Fatal("stopped instance contacted upstream")
			}
			if lifecycle != "shutdown" {
				config = mustMarshal(t, LifecycleRequest{ConfigYAML: []byte(fmt.Sprintf("enabled: true\nstate_file: %q\npause_reset_follow_sync: true\n", path))})
				if _, err := app.HandleMethod(MethodPluginReconfigure, config); err != nil {
					t.Fatal(err)
				}
				if queries.Load() != 1 {
					t.Fatal("paused reconfiguration started an unsolicited quota query")
				}
				view, _ := app.store.KeyViewForScope(billing.CallerScope("sk-dummy-follow-a"))
				if view.ResetFollow == nil {
					t.Fatal("lifecycle discarded follow settings")
				}
			}
		})
	}
}

func TestResetFollowManualSuccessFailureAndIdempotence(t *testing.T) {
	for _, outcome := range []string{"success", "refresh failure", "reset failure"} {
		t.Run(outcome, func(t *testing.T) {
			app, _, file, windowID := resetFollowApp(t)
			var consumes atomic.Int32
			app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
				if req.Method == http.MethodPost {
					consumes.Add(1)
					if outcome == "reset failure" {
						return nil, errors.New("dummy reset failure")
					}
					return json.Marshal(hostHTTPResponse{StatusCode: 200})
				}
				if outcome == "refresh failure" {
					return nil, errors.New("dummy refresh failure")
				}
				return resetQuotaResponse(), nil
			}))
			a, b := billing.CallerScope("sk-dummy-follow-a"), billing.CallerScope("sk-dummy-follow-b")
			record := func(scope string) {
				now := app.store.Now()
				app.store.RecordUsage(billing.UsageEvent{Scope: scope, UpstreamModel: "gpt-5.5", At: now, RequestedAt: now})
			}
			used := func(scope string) string {
				view, _ := app.store.KeyViewForScope(scope)
				return view.Windows[0].Dimensions[0].Used.String()
			}
			record(a)
			record(b)
			req := ManagementRequest{Headers: http.Header{"X-Quota-Reset-Id": {"12345678-1234-4234-8234-123456789abc"}}, Query: map[string][]string{"auth_index": {file.AuthIndex}, "auth_name": {file.Name}, "auth_revision": {""}}}
			response := app.authQuotaReset(req, viewAccess{})
			if outcome == "reset failure" {
				if response.StatusCode != 502 || used(a) != "1" || used(b) != "1" {
					t.Fatalf("failed reset changed followers: %s", response.Body)
				}
				return
			}
			if response.StatusCode != 200 || used(a) != "0" || used(b) != "0" {
				t.Fatalf("reset did not reach all followers: %s", response.Body)
			}
			view, _ := app.store.KeyViewForScope(a)
			if outcome == "refresh failure" {
				if !strings.Contains(string(response.Body), "refresh_error") || !view.ResetFollow.Windows[windowID].NextResetAt.IsZero() {
					t.Fatalf("refresh failure obscured success: %s %+v", response.Body, view)
				}
			}
			record(a)
			response = app.authQuotaReset(req, viewAccess{})
			if response.StatusCode != 200 || consumes.Load() != 1 || used(a) != "1" {
				t.Fatalf("repeated operation reset twice: %s", response.Body)
			}
		})
	}
}

func TestResetFollowQueriesSerializeAndDoNotBlockBilling(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var active, maximum atomic.Int32
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		count := active.Add(1)
		if count > maximum.Load() {
			maximum.Store(count)
		}
		defer active.Add(-1)
		if strings.HasSuffix(req.URL, "/usage") {
			entered <- struct{}{}
			<-release
		}
		return resetQuotaResponse(), nil
	}))
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			app.authQuota(ManagementRequest{Query: map[string][]string{"auth_index": {file.AuthIndex}}}, viewAccess{})
		}()
	}
	waitFollowSignal(t, entered)
	scope := billing.CallerScope("sk-dummy-follow-a")
	// The callback is blocked, but billing and config writes still complete.
	if !app.store.Authorize(scope, app.store.Now()).Allowed {
		t.Fatal("network query blocked billing")
	}
	if err := app.store.SetResetFollow(scope, ""); err != nil {
		t.Fatal(err)
	}
	close(release)
	group.Wait()
	view, _ := app.store.KeyViewForScope(scope)
	if view.ResetFollow != nil || maximum.Load() != 1 {
		t.Fatal("query overwrote configuration or overlapped")
	}
}

func TestResetFollowParserUsesOnlyExplicitOrdinaryWindows(t *testing.T) {
	app := newConfiguredApp(t)
	file := hostAuthFile{ID: "dummy-id", AuthIndex: "dummy-index", Type: "claude"}
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		body := fmt.Sprintf(`{"five_hour":{"resets_at":%q},"seven_day":{"resets_at":%q},"seven_day_sonnet":{"resets_at":%q}}`, time.Now().Add(time.Hour).Format(time.RFC3339), time.Now().Add(24*time.Hour).Format(time.RFC3339), time.Now().Add(24*time.Hour).Format(time.RFC3339))
		return json.Marshal(hostHTTPResponse{StatusCode: 200, Body: []byte(body)})
	}))
	result, err := app.fetchAuthQuota("", file, "claude")
	if err != nil || len(result.Quota) != 3 || !result.Quota[0].Ordinary || !result.Quota[1].Ordinary || result.Quota[2].Ordinary || result.Quota[0].PeriodSeconds != 18000 || result.Quota[1].PeriodSeconds != 604800 {
		t.Fatalf("Claude windows: %+v %v", result, err)
	}
	for _, prefix := range []string{"", "Code Review ", "Model "} {
		for _, seconds := range []float64{0, 18000, 18000.5} {
			result = authQuotaResponse{}
			appendCodexRateLimit(&result, prefix, map[string]any{"primary_window": map[string]any{"limit_window_seconds": seconds}})
			row := result.Quota[0]
			if row.Ordinary != (prefix == "") || (row.PeriodSeconds == 18000) != (seconds == 18000) {
				t.Fatalf("Codex metadata: %+v", row)
			}
		}
	}
}

func TestResetFollowEnableAndDisableHaveNoDeferredWork(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	var queries atomic.Int32
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		if req.HostCallbackID != "setting-callback" {
			t.Errorf("unexpected callback context: %q", req.HostCallbackID)
		}
		if strings.HasSuffix(req.URL, "/usage") {
			queries.Add(1)
		}
		return resetQuotaResponse(), nil
	}))
	for _, index := range []string{file.AuthIndex, ""} {
		response := followManagementCall(t, app, ManagementRequest{Method: http.MethodPut, Path: managementBase + routeKeysResetFollow,
			HostCallbackID: "setting-callback", Body: mustMarshal(t, map[string]string{"scope": billing.CallerScope("sk-dummy-follow-a"), "auth_index": index})})
		if response.StatusCode != 200 || queries.Load() != 1 {
			t.Fatalf("unexpected setting query count: %d, response: %s", queries.Load(), response.Body)
		}
	}
	app.Shutdown()
	if queries.Load() != 1 {
		t.Fatal("setting left deferred upstream work")
	}
}

func TestResetFollowSubscriptionMasksDiagnostics(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	app.store.ApplyResetSnapshot(billing.ResetSnapshot{AuthIndex: file.AuthIndex, AttemptedAt: app.store.Now(), Error: billing.ResetError(messages.Literal("dummy@example.com unavailable"))})
	view, _ := app.store.KeyViewForScope(billing.CallerScope("sk-dummy-follow-a"))
	response := app.accountSubscription(viewAccess{APIKey: true, Tracked: true, MaskEmails: true, Key: view})
	if strings.Contains(string(response.Body), "dummy@example.com") || !strings.Contains(string(response.Body), "du**y@example.com") {
		t.Fatalf("unmasked diagnostics: %s", response.Body)
	}
	view, _ = app.store.KeyViewForScope(view.Scope)
	if !strings.Contains(view.ResetFollow.Error.Text, "dummy@example.com") {
		t.Fatal("masking changed stored error")
	}
}

func TestResetFollowManualResetWaitsForOlderQuery(t *testing.T) {
	app, _, file, windowID := resetFollowApp(t)
	queryEntered, queryRelease := make(chan struct{}), make(chan struct{})
	consumed := make(chan struct{})
	var queries atomic.Int32
	oldBoundary, newBoundary := time.Now().Add(time.Hour).Truncate(time.Second), time.Now().Add(3*time.Hour).Truncate(time.Second)
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		if req.Method == http.MethodPost {
			close(consumed)
			return json.Marshal(hostHTTPResponse{StatusCode: 200})
		}
		boundary := newBoundary
		if strings.HasSuffix(req.URL, "/usage") && queries.Add(1) == 1 {
			close(queryEntered)
			<-queryRelease
			boundary = oldBoundary
		}
		body := fmt.Sprintf(`{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"reset_at":%d}}}`, boundary.Unix())
		return json.Marshal(hostHTTPResponse{StatusCode: 200, Body: []byte(body)})
	}))
	req := ManagementRequest{Query: map[string][]string{"auth_index": {file.AuthIndex}, "auth_name": {file.Name}, "auth_revision": {""}}, Headers: http.Header{"X-Quota-Reset-Id": {"12345678-1234-4234-8234-123456789abc"}}}
	queried, reset := make(chan struct{}), make(chan struct{})
	go func() { app.authQuota(req, viewAccess{}); close(queried) }()
	waitFollowSignal(t, queryEntered)
	go func() { app.authQuotaReset(req, viewAccess{}); close(reset) }()
	select {
	case <-consumed:
		t.Fatal("manual reset overtook an older query")
	case <-time.After(25 * time.Millisecond):
	}
	close(queryRelease)
	waitFollowSignal(t, queried)
	waitFollowSignal(t, reset)
	view, _ := app.store.KeyViewForScope(billing.CallerScope("sk-dummy-follow-a"))
	if !view.ResetFollow.Windows[windowID].NextResetAt.Equal(newBoundary) {
		t.Fatalf("old query overwrote manual reset: %+v", view.ResetFollow)
	}
}

func TestResetFollowSubscriptionRefreshIsCallerScoped(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	other := billing.CallerScope("sk-dummy-follow-b")
	now := app.store.Now()
	app.store.ApplyResetSnapshot(billing.ResetSnapshot{AuthIndex: "other-host-index", Provider: "codex", CredentialRef: billing.CredentialFingerprint("other-host-id"), AttemptedAt: now, SyncedAt: now,
		Windows: []billing.UpstreamWindow{{ID: "primary_window", PeriodSeconds: 18000, ResetAt: now.Add(time.Hour)}}})
	if err := app.store.SetResetFollow(other, "other-host-index"); err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int32
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		if req.HostCallbackID != "subscription-callback" {
			t.Errorf("unexpected callback context: %q", req.HostCallbackID)
		}
		if strings.HasSuffix(req.URL, "/usage") {
			queries.Add(1)
		}
		return resetQuotaResponse(), nil
	}))
	req := ManagementRequest{Method: http.MethodGet, Path: resourceBase + routeSubscription, HostCallbackID: "subscription-callback",
		Headers: http.Header{"Authorization": {"Bearer sk-dummy-follow-a"}},
		Query:   map[string][]string{"refresh_reset_follow": {"1"}, "auth_index": {"other-host-index"}}}
	response := followManagementCall(t, app, req)
	var result accountSubscriptionResponse
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || queries.Load() != 1 || !result.Subscription.ResetFollow.SyncedAt.After(now) {
		t.Fatalf("subscription did not return refreshed state: %s", response.Body)
	}
	untouched, _ := app.store.KeyViewForScope(other)
	if !untouched.ResetFollow.SyncedAt.Equal(now) || untouched.ResetFollow.Error.Text != "" {
		t.Fatal("account refresh affected another account")
	}
	req.Headers.Set("Authorization", "Bearer sk-dummy-untracked")
	response = followManagementCall(t, app, req)
	if response.StatusCode != http.StatusUnauthorized || queries.Load() != 1 {
		t.Fatal("untracked caller triggered upstream synchronization")
	}
}

func TestResetFollowPageRefreshFailureKeepsQuotaVisible(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	app.SetHostCaller(resetFollowHost(t, file, func(hostHTTPRequest) (json.RawMessage, error) {
		return nil, errors.New("dummy refresh unavailable")
	}))
	scope := billing.CallerScope("sk-dummy-follow-a")
	now := app.store.Now()
	app.store.RecordUsage(billing.UsageEvent{Scope: scope, UpstreamModel: "gpt-5.5", At: now, RequestedAt: now})
	response := followManagementCall(t, app, ManagementRequest{Method: http.MethodGet, Path: managementBase + routeKeys,
		Query: map[string][]string{"refresh_reset_follow": {"1"}}})
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), "dummy refresh unavailable") {
		t.Fatalf("failed refresh hid the quota view: %s", response.Body)
	}
	view, _ := app.store.KeyViewForScope(scope)
	if view.Windows[0].Dimensions[0].Used.String() != "1" || view.ResetFollow.Windows[view.Windows[0].ID].NextResetAt.IsZero() {
		t.Fatal("failed refresh discarded usage or the confirmed boundary")
	}
}
