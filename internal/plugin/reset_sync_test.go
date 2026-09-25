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
)

type resetTestClock struct {
	mu        sync.Mutex
	now       time.Time
	timers    []*resetTestTimer
	updates   chan struct{}
	intervals []time.Duration
}
type resetTestTimer struct {
	clock    *resetTestClock
	channel  chan time.Time
	deadline time.Time
	stopped  bool
}

func newResetTestClock() *resetTestClock {
	return &resetTestClock{now: time.Now(), updates: make(chan struct{}, 32)}
}
func (c *resetTestClock) NewTimer(d time.Duration) resetSyncTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &resetTestTimer{clock: c, channel: make(chan time.Time, 1), deadline: c.now.Add(d)}
	c.timers = append(c.timers, timer)
	c.intervals = append(c.intervals, d)
	c.updates <- struct{}{}
	return timer
}
func (t *resetTestTimer) C() <-chan time.Time { return t.channel }
func (t *resetTestTimer) Reset(d time.Duration) {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.deadline = t.clock.now.Add(d)
	t.stopped = false
	t.clock.intervals = append(t.clock.intervals, d)
	t.clock.updates <- struct{}{}
}
func (t *resetTestTimer) Stop() {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.stopped = true
}
func (c *resetTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, timer := range c.timers {
		if !timer.stopped && !timer.deadline.IsZero() && !timer.deadline.After(c.now) {
			timer.channel <- c.now
			timer.deadline = time.Time{}
		}
	}
}
func (c *resetTestClock) checkStopped(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		if !timer.stopped {
			t.Fatal("timer survived lifecycle stop")
		}
	}
	for _, interval := range c.intervals {
		if interval != 30*time.Minute {
			t.Fatalf("interval = %v", interval)
		}
	}
}
func currentResetWorker(app *App) *resetSyncWorker {
	app.resetSyncMu.Lock()
	defer app.resetSyncMu.Unlock()
	return app.resetSync
}
func startResetTestWorker(app *App, immediate bool) {
	app.callsMu.RLock()
	defer app.callsMu.RUnlock()
	app.startResetSync(immediate)
}
func resetSyncConfig(t *testing.T, app *App, path, extra string) {
	t.Helper()
	raw, err := app.HandleMethod(MethodPluginReconfigure, mustMarshal(t, LifecycleRequest{ConfigYAML: []byte(fmt.Sprintf("enabled: true\nstate_file: %q\n%s", path, extra))}))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, raw, nil)
}

func TestResetSyncIdleScheduleDeduplicatesAndPreservesUsage(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	clock := newResetTestClock()
	app.newResetSyncTimer = clock.NewTimer
	var queries atomic.Int32
	var failing atomic.Bool
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		if strings.HasSuffix(req.URL, "/usage") {
			queries.Add(1)
			if failing.Load() {
				return nil, errors.New("dummy scheduled query failure")
			}
		}
		return resetQuotaResponse(), nil
	}))
	scope := billing.CallerScope("sk-dummy-follow-a")
	now := app.store.Now()
	app.store.RecordUsage(billing.UsageEvent{Scope: scope, UpstreamModel: "gpt-5.5", At: now, RequestedAt: now})
	startResetTestWorker(app, false)
	waitFollowSignal(t, clock.updates)
	clock.Advance(30*time.Minute - time.Second)
	if queries.Load() != 0 {
		t.Fatal("queried before thirty minutes")
	}
	clock.Advance(time.Second)
	waitFollowSignal(t, clock.updates)
	if queries.Load() != 1 {
		t.Fatal("idle followers did not share one query")
	}
	view, _ := app.store.KeyViewForScope(scope)
	if view.Windows[0].Dimensions[0].Used.String() != "1" {
		t.Fatal("scheduled refresh cleared usage")
	}
	// A failed background round leaves the same confirmed boundary and usage.
	failing.Store(true)
	clock.Advance(30 * time.Minute)
	waitFollowSignal(t, clock.updates)
	view, _ = app.store.KeyViewForScope(scope)
	if queries.Load() != 2 || view.ResetFollow.Error.Text == "" || view.Windows[0].Dimensions[0].Used.String() != "1" {
		t.Fatal("failed round lost state")
	}
	failing.Store(false)
	clock.Advance(30 * time.Minute)
	waitFollowSignal(t, clock.updates)
	view, _ = app.store.KeyViewForScope(scope)
	if queries.Load() != 3 || view.ResetFollow.Error.Text != "" || view.Windows[0].Dimensions[0].Used.String() != "1" {
		t.Fatal("recovery did not preserve usage")
	}
	for _, key := range []string{"sk-dummy-follow-a", "sk-dummy-follow-b"} {
		if err := app.store.SetResetFollow(billing.CallerScope(key), ""); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(30 * time.Minute)
	waitFollowSignal(t, clock.updates)
	if queries.Load() != 3 {
		t.Fatal("queried an account with no remaining followers")
	}
	app.Shutdown()
	clock.checkStopped(t)
}

func TestResetSyncLifecycleWithSlowRound(t *testing.T) {
	for _, operation := range []string{"pause", "disable", "quiesce", "shutdown", "switch storage"} {
		t.Run(operation, func(t *testing.T) {
			app, path, file, _ := resetFollowApp(t)
			clock := newResetTestClock()
			app.newResetSyncTimer = clock.NewTimer
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			var queries atomic.Int32
			app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
				if strings.HasSuffix(req.URL, "/usage") && queries.Add(1) == 1 {
					close(entered)
					<-release
				}
				return resetQuotaResponse(), nil
			}))
			startResetTestWorker(app, false)
			waitFollowSignal(t, clock.updates)
			worker := currentResetWorker(app)
			clock.Advance(30 * time.Minute)
			waitFollowSignal(t, entered)
			// No timer is armed during a round, even if several intervals pass.
			clock.Advance(2 * time.Hour)
			if queries.Load() != 1 {
				t.Fatal("overlapping scheduled rounds")
			}
			billed := make(chan struct{})
			go func() {
				scope := billing.CallerScope("sk-dummy-follow-a")
				now := app.store.Now()
				app.store.Authorize(scope, now)
				app.store.RecordUsage(billing.UsageEvent{Scope: scope, UpstreamModel: "gpt-5.5", At: now, RequestedAt: now})
				close(billed)
			}()
			waitFollowSignal(t, billed)
			config := fmt.Sprintf("enabled: true\nstate_file: %q\npause_reset_follow_sync: true\n", path)
			if operation == "disable" {
				config = fmt.Sprintf("enabled: false\nstate_file: %q\n", path)
			}
			if operation == "switch storage" {
				config = fmt.Sprintf("enabled: true\nstate_file: %q\n", t.TempDir()+"/replacement.db")
			}
			raw := mustMarshal(t, LifecycleRequest{ConfigYAML: []byte(config)})
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				switch operation {
				case "shutdown":
					app.Shutdown()
				case "quiesce":
					_, err := app.HandleMethod(MethodPluginQuiesce, nil)
					if err != nil {
						t.Error(err)
					}
				default:
					_, err := app.HandleMethod(MethodPluginReconfigure, raw)
					if err != nil {
						t.Error(err)
					}
				}
			}()
			if operation == "pause" || operation == "disable" {
				// Settings on the open database apply without joining the issued query.
				waitFollowSignal(t, finished)
			} else {
				select {
				case <-finished:
					t.Fatal("lifecycle abandoned an issued host callback")
				case <-time.After(25 * time.Millisecond):
				}
			}
			once.Do(func() { close(release) })
			waitFollowSignal(t, finished)
			waitFollowSignal(t, worker.done)
			clock.checkStopped(t)
			clock.Advance(24 * time.Hour)
			if queries.Load() != 1 {
				t.Fatal("stopped worker issued another query")
			}
			if operation == "pause" {
				view, _ := app.store.KeyViewForScope(billing.CallerScope("sk-dummy-follow-a"))
				if !view.ResetFollowSyncPaused || view.Windows[0].Dimensions[0].Used.String() != "1" {
					t.Fatal("pause changed counters or was not exposed")
				}
				response := app.accountSubscription(viewAccess{Tracked: true, Key: view})
				if !strings.Contains(string(response.Body), `"reset_follow_sync_paused":true`) {
					t.Fatal("subscription omitted pause status")
				}
				app.refreshResetFollowers(ManagementRequest{}, viewAccess{})
				if queries.Load() != 2 {
					t.Fatal("pause prevented manual refresh")
				}
			}
			if operation != "shutdown" {
				resetSyncConfig(t, app, path, "")
				waitFollowSignal(t, clock.updates)
				expected := int32(2)
				if operation == "pause" {
					expected = 3
				}
				if queries.Load() != expected {
					t.Fatalf("resume did not immediately synchronize persisted followers: %d", queries.Load())
				}
				view, _ := app.store.KeyViewForScope(billing.CallerScope("sk-dummy-follow-a"))
				if view.ResetFollow == nil || view.ResetFollowSyncPaused || view.Windows[0].Dimensions[0].Used.String() != "1" {
					t.Fatal("resume lost settings or usage")
				}
			}
		})
	}
}

func TestResetSyncSharesAccountLockWithPageRefreshAndUsesNoExpiredContext(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	clock := newResetTestClock()
	app.newResetSyncTimer = clock.NewTimer
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var queries, active, maxActive atomic.Int32
	var sawPage, sawBackground atomic.Bool
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		n := active.Add(1)
		defer active.Add(-1)
		if n > maxActive.Load() {
			maxActive.Store(n)
		}
		if strings.HasSuffix(req.URL, "/usage") {
			switch req.HostCallbackID {
			case "":
				sawBackground.Store(true)
			case "page-context":
				sawPage.Store(true)
			default:
				t.Errorf("unexpected context: %q", req.HostCallbackID)
			}
			if queries.Add(1) == 1 {
				close(entered)
				<-release
			}
		}
		return resetQuotaResponse(), nil
	}))
	startResetTestWorker(app, false)
	waitFollowSignal(t, clock.updates)
	pageRequest := mustMarshal(t, ManagementRequest{Method: http.MethodGet, Path: managementBase + routeKeys, HostCallbackID: "page-context", Query: map[string][]string{"refresh_reset_follow": {"1"}}})
	pageDone := make(chan struct{})
	go func() { defer close(pageDone); _, _ = app.HandleMethod(MethodManagementHandle, pageRequest) }()
	waitFollowSignal(t, entered)
	clock.Advance(30 * time.Minute)
	once.Do(func() { close(release) })
	waitFollowSignal(t, pageDone)
	waitFollowSignal(t, clock.updates)
	if queries.Load() != 2 || maxActive.Load() != 1 || !sawPage.Load() || !sawBackground.Load() {
		t.Fatal("foreground and background account coordination failed")
	}
}

func TestResetSyncFirstEnableStartsOneTimerWithoutDuplicateQuery(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	clock := newResetTestClock()
	app.newResetSyncTimer = clock.NewTimer
	for _, key := range []string{"sk-dummy-follow-a", "sk-dummy-follow-b"} {
		if err := app.store.SetResetFollow(billing.CallerScope(key), ""); err != nil {
			t.Fatal(err)
		}
	}
	var queries atomic.Int32
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		if strings.HasSuffix(req.URL, "/usage") {
			queries.Add(1)
		}
		return resetQuotaResponse(), nil
	}))
	startResetTestWorker(app, true)
	if currentResetWorker(app) != nil {
		t.Fatal("started worker without followers")
	}
	for _, key := range []string{"sk-dummy-follow-a", "sk-dummy-follow-b"} {
		response := followManagementCall(t, app, ManagementRequest{Method: http.MethodPut, Path: managementBase + routeKeysResetFollow, Body: mustMarshal(t, map[string]string{"scope": billing.CallerScope(key), "auth_index": file.AuthIndex})})
		if response.StatusCode != 200 {
			t.Fatalf("enable: %s", response.Body)
		}
	}
	waitFollowSignal(t, clock.updates)
	if queries.Load() != 2 {
		t.Fatal("save caused duplicate background query")
	}
	clock.mu.Lock()
	timers := len(clock.timers)
	clock.mu.Unlock()
	if timers != 1 {
		t.Fatal("created more than one scheduler")
	}
	clock.Advance(30 * time.Minute)
	waitFollowSignal(t, clock.updates)
	if queries.Load() != 3 {
		t.Fatal("followers did not share the scheduled query")
	}
}

func TestResetSyncRejectedConfigurationKeepsSchedule(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	clock := newResetTestClock()
	app.newResetSyncTimer = clock.NewTimer
	var queries atomic.Int32
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		if strings.HasSuffix(req.URL, "/usage") {
			queries.Add(1)
		}
		return resetQuotaResponse(), nil
	}))
	startResetTestWorker(app, false)
	waitFollowSignal(t, clock.updates)
	previous := currentResetWorker(app)
	if _, err := app.HandleMethod(MethodPluginReconfigure, mustMarshal(t, LifecycleRequest{ConfigYAML: []byte("enabled: invalid-boolean")})); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	if currentResetWorker(app) != previous || resetSyncStopped(previous.stop) || queries.Load() != 0 {
		t.Fatal("rejected configuration disturbed the running schedule")
	}
	// A database that cannot be opened keeps the previous one and restarts it.
	unusable := mustMarshal(t, LifecycleRequest{ConfigYAML: []byte(fmt.Sprintf("enabled: true\nstate_file: %q\n", t.TempDir()))})
	if _, err := app.HandleMethod(MethodPluginReconfigure, unusable); err == nil {
		t.Fatal("unusable database accepted")
	}
	waitFollowSignal(t, previous.done)
	waitFollowSignal(t, clock.updates)
	if queries.Load() != 1 {
		t.Fatal("previous schedule was not restarted after a failed storage switch")
	}
	clock.Advance(30 * time.Minute)
	waitFollowSignal(t, clock.updates)
	if queries.Load() != 2 {
		t.Fatal("timer did not resume")
	}
}

func TestResetSyncStopSkipsRemainingAccounts(t *testing.T) {
	app, _, file, _ := resetFollowApp(t)
	now := app.store.Now()
	app.store.ApplyResetSnapshot(billing.ResetSnapshot{AuthIndex: "zz-other-account", Provider: "codex", CredentialRef: billing.CredentialFingerprint("other-host-id"), AttemptedAt: now, SyncedAt: now, Windows: []billing.UpstreamWindow{{ID: "primary", PeriodSeconds: 18000, ResetAt: now.Add(time.Hour)}}})
	if err := app.store.SetResetFollow(billing.CallerScope("sk-dummy-follow-b"), "zz-other-account"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var queries atomic.Int32
	caller := resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		if strings.HasSuffix(req.URL, "/usage") && queries.Add(1) == 1 {
			close(entered)
			<-release
		}
		return resetQuotaResponse(), nil
	})
	app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		if method == hostAuthList {
			other := file
			other.ID = "other-host-id"
			other.AuthIndex = "zz-other-account"
			return json.Marshal(hostAuthListResponse{Files: []hostAuthFile{file, other}})
		}
		return caller(method, payload)
	})
	startResetTestWorker(app, true)
	waitFollowSignal(t, entered)
	worker := currentResetWorker(app)
	stopped := make(chan struct{})
	go func() { app.Shutdown(); close(stopped) }()
	waitFollowSignal(t, worker.stop)
	once.Do(func() { close(release) })
	waitFollowSignal(t, stopped)
	if queries.Load() != 1 {
		t.Fatal("shutdown started another account after joining the issued query")
	}
}

func TestResetSyncResumeWhileQueryHangsKeepsOneWorker(t *testing.T) {
	app, path, file, _ := resetFollowApp(t)
	clock := newResetTestClock()
	app.newResetSyncTimer = clock.NewTimer
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var queries, active, maxActive atomic.Int32
	app.SetHostCaller(resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		if strings.HasSuffix(req.URL, "/usage") {
			if n := active.Add(1); n > maxActive.Load() {
				maxActive.Store(n)
			}
			defer active.Add(-1)
			if queries.Add(1) == 1 {
				close(entered)
				<-release
			}
		}
		return resetQuotaResponse(), nil
	}))
	startResetTestWorker(app, true)
	waitFollowSignal(t, entered)
	worker := currentResetWorker(app)
	// The upstream never answers the issued query; settings still apply at once.
	for _, extra := range []string{"pause_reset_follow_sync: true\n", ""} {
		applied := make(chan struct{})
		config := mustMarshal(t, LifecycleRequest{ConfigYAML: []byte(fmt.Sprintf("enabled: true\nstate_file: %q\n%s", path, extra))})
		go func() {
			defer close(applied)
			if err := configureApp(app, config); err != nil {
				t.Error(err)
			}
		}()
		waitFollowSignal(t, applied)
	}
	if currentResetWorker(app) != worker {
		t.Fatal("resuming replaced the busy worker")
	}
	once.Do(func() { close(release) })
	// The same worker runs the resumed round after its query, then arms its timer.
	waitFollowSignal(t, clock.updates)
	clock.mu.Lock()
	timers := len(clock.timers)
	clock.mu.Unlock()
	if queries.Load() != 2 || maxActive.Load() != 1 || timers != 1 {
		t.Fatalf("queries = %d, concurrent = %d, schedulers = %d", queries.Load(), maxActive.Load(), timers)
	}
}

func TestResetSyncStartupWaitsForHostAuthInventory(t *testing.T) {
	app, path, file, _ := resetFollowApp(t)
	app.Shutdown()
	restarted := newTestApp(t)
	t.Cleanup(restarted.Shutdown)
	clock := newResetTestClock()
	restarted.newResetSyncTimer = clock.NewTimer
	var loaded atomic.Bool
	var queries atomic.Int32
	caller := resetFollowHost(t, file, func(req hostHTTPRequest) (json.RawMessage, error) {
		if strings.HasSuffix(req.URL, "/usage") {
			queries.Add(1)
		}
		return resetQuotaResponse(), nil
	})
	restarted.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		if method == hostAuthList && !loaded.Load() {
			// Before CPA attaches its auth manager it lists files from disk, without indexes.
			return json.Marshal(hostAuthListResponse{Files: []hostAuthFile{{Name: file.Name, Type: file.Type, Source: "file"}}})
		}
		return caller(method, payload)
	})
	config := mustMarshal(t, LifecycleRequest{ConfigYAML: []byte(fmt.Sprintf("enabled: true\nstate_file: %q\n", path))})
	if _, err := restarted.HandleMethod(MethodPluginRegister, config); err != nil {
		t.Fatal(err)
	}
	waitFollowSignal(t, clock.updates)
	// CPA reconfigures again before it loads auth files.
	if err := configureApp(restarted, config); err != nil {
		t.Fatal(err)
	}
	waitFollowSignal(t, clock.updates)
	view, _ := restarted.store.KeyViewForScope(billing.CallerScope("sk-dummy-follow-a"))
	if queries.Load() != 0 || view.ResetFollow.Error.Text != "" {
		t.Fatalf("startup reported followers before the host loaded them: queries = %d, error = %q", queries.Load(), view.ResetFollow.Error.Text)
	}
	// The reconfiguration after loading runs the owed synchronization once.
	loaded.Store(true)
	if err := configureApp(restarted, config); err != nil {
		t.Fatal(err)
	}
	waitFollowSignal(t, clock.updates)
	view, _ = restarted.store.KeyViewForScope(billing.CallerScope("sk-dummy-follow-a"))
	if queries.Load() != 1 || view.ResetFollow.Error.Text != "" || view.ResetFollow.SyncedAt.IsZero() {
		t.Fatalf("owed synchronization: queries = %d, follow = %+v", queries.Load(), view.ResetFollow)
	}
	if err := configureApp(restarted, config); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if queries.Load() != 1 {
		t.Fatal("a later configuration save queried again")
	}
}
