package billing

import (
	"strings"
	"testing"
	"time"
)

const (
	testFiveHours = int64(5 * 60 * 60)
	testWeek      = int64(7 * 24 * 60 * 60)
)

var testUpstreamCredential = CredentialFingerprint("codex-user.json")

func TestClassifyUpstreamWindow(t *testing.T) {
	week := time.Duration(testWeek) * time.Second
	observedAt := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	end := observedAt.Add(3 * 24 * time.Hour)
	baseline := &UpstreamWindow{PeriodSeconds: testWeek, ResetAt: end, UsedPercent: 80, ObservedAt: observedAt}
	at := observedAt.Add(5 * time.Minute)
	for _, test := range []struct {
		name        string
		previous    *UpstreamWindow
		observation UpstreamObservation
		at          time.Time
		wantReset   bool
		wantResetAt time.Time
		wantEnd     time.Time // zero means no baseline
	}{
		{name: "first started observation", observation: UpstreamObservation{testWeek, end, 80}, at: at, wantEnd: end},
		{name: "first idle observation", observation: UpstreamObservation{testWeek, at.Add(week), 0}, at: at},
		{name: "same window with jitter", previous: baseline, observation: UpstreamObservation{testWeek, end.Add(30 * time.Second), 81}, at: at, wantEnd: end},
		{name: "counter cleared in the same window", previous: baseline, observation: UpstreamObservation{testWeek, end, 3}, at: at, wantReset: true, wantResetAt: at, wantEnd: end},
		{name: "raised limit halves usage", previous: baseline, observation: UpstreamObservation{testWeek, end, 40}, at: at, wantEnd: end},
		{name: "small usage cleared", previous: &UpstreamWindow{PeriodSeconds: testWeek, ResetAt: end, UsedPercent: 4, ObservedAt: observedAt}, observation: UpstreamObservation{testWeek, end, 0}, at: at, wantEnd: end},
		{
			name: "window restarted before its end", previous: baseline,
			observation: UpstreamObservation{testWeek, at.Add(week - 2*time.Minute), 1}, at: at,
			wantReset: true, wantResetAt: at.Add(-2 * time.Minute), wantEnd: at.Add(week - 2*time.Minute),
		},
		{
			name: "restart time before the previous observation", previous: baseline,
			observation: UpstreamObservation{testWeek, observedAt.Add(week - time.Hour), 1}, at: at,
			wantReset: true, wantResetAt: at, wantEnd: observedAt.Add(week - time.Hour),
		},
		{name: "window cleared before its end", previous: baseline, observation: UpstreamObservation{testWeek, at.Add(week), 0}, at: at, wantReset: true, wantResetAt: at},
		{name: "window expired while idle", previous: baseline, observation: UpstreamObservation{testWeek, end.Add(time.Hour + week), 0}, at: end.Add(time.Hour)},
		{name: "scheduled rollover", previous: baseline, observation: UpstreamObservation{testWeek, end.Add(2*time.Hour + week), 5}, at: end.Add(3 * time.Hour), wantEnd: end.Add(2*time.Hour + week)},
		{name: "reset time moved earlier", previous: baseline, observation: UpstreamObservation{testWeek, end.Add(-time.Hour), 80}, at: at, wantEnd: end.Add(-time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			next, resetAt, reset := classifyUpstreamWindow(test.previous, test.observation, test.at)
			if reset != test.wantReset || reset && !resetAt.Equal(test.wantResetAt) {
				t.Fatalf("reset = %v at %v, want %v at %v", reset, resetAt, test.wantReset, test.wantResetAt)
			}
			if test.wantEnd.IsZero() != (next == nil) || next != nil && (!next.ResetAt.Equal(test.wantEnd) || !next.ObservedAt.Equal(test.at)) {
				t.Fatalf("baseline = %+v, want end %v", next, test.wantEnd)
			}
		})
	}
}

func followingStore(t *testing.T, clock *time.Time, windows []QuotaWindow) *Store {
	t.Helper()
	store := newAccountStore(t, *clock)
	store.now = func() time.Time { return *clock }
	store.ReplaceAll(func(state *State) {
		state.Keys["a"] = &KeyState{}
		state.Keys["b"] = &KeyState{}
		state.Keys["other"] = &KeyState{}
	})
	if _, err := store.CreatePlanWithBindings(Plan{ID: "p", Name: "Codex", Windows: windows,
		UpstreamReset: &UpstreamReset{Credential: " " + strings.ToUpper(testUpstreamCredential) + " ", LastResetAt: *clock}}, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePlanWithBindings(Plan{ID: "other", Windows: []QuotaWindow{{Name: "Weekly", AmountUSD: 1, PeriodSeconds: testWeek}}}, []string{"other"}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"a", "b", "other"} {
		store.RecordUsage(admittedEvent(store, scope, *clock))
	}
	return store
}

func TestIndependentPlanFollowsEarlyUpstreamReset(t *testing.T) {
	now := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	clock := now
	store := followingStore(t, &clock, []QuotaWindow{
		{Name: "5h", AmountUSD: 1, PeriodSeconds: testFiveHours},
		{Name: "Daily", AmountUSD: 1, PeriodSeconds: 86400},
		{Name: "Weekly", AmountUSD: 0.001, PeriodSeconds: testWeek},
	})
	var plan Plan
	store.Read(func(state *State) { plan, _ = state.FindPlan("p") })
	if plan.UpstreamReset == nil || plan.UpstreamReset.Credential != testUpstreamCredential || !plan.UpstreamReset.LastResetAt.IsZero() {
		t.Fatalf("created follow = %+v, want only the normalized credential", plan.UpstreamReset)
	}
	if store.Authorize("a", now).Allowed {
		t.Fatal("the weekly quota should be exhausted")
	}
	weekEnd := now.Add(3 * 24 * time.Hour)
	fiveHourEnd := now.Add(2 * time.Hour)
	observe := func(week, fiveHour time.Time, weekUsed float64) []UpstreamFollowResult {
		return store.ObserveUpstream(testUpstreamCredential, clock, []UpstreamObservation{
			{PeriodSeconds: testFiveHours, ResetAt: fiveHour, UsedPercent: 10},
			{PeriodSeconds: testWeek, ResetAt: week, UsedPercent: weekUsed},
		})
	}
	clock = now.Add(time.Minute)
	if results := observe(weekEnd, fiveHourEnd, 90); len(results) != 0 {
		t.Fatalf("baseline observation changed quotas: %+v", results)
	}
	clock = now.Add(6 * time.Minute)
	restart := clock.Add(-time.Minute)
	results := observe(restart.Add(time.Duration(testWeek)*time.Second), fiveHourEnd, 1)
	if len(results) != 1 || strings.Join(results[0].ResetWindows, ",") != "Weekly" || results[0].ResetKeys != 2 || len(results[0].AlignedWindows) != 0 {
		t.Fatalf("results = %+v, want the weekly window reset for both keys", results)
	}
	store.Read(func(state *State) {
		for _, scope := range []string{"a", "b"} {
			key := state.Keys[scope]
			if _, exists := key.Cycles[plan.Windows[2].ID]; exists || len(key.Cycles) != 2 {
				t.Fatalf("%s cycles = %+v, want only the weekly window cleared", scope, key.Cycles)
			}
		}
		if len(state.Keys["other"].Cycles) != 1 {
			t.Fatal("a plan that does not follow the credential was reset")
		}
		follow := state.Plans[0].UpstreamReset
		if !follow.LastResetAt.Equal(clock) || len(follow.Windows) != 2 {
			t.Fatalf("follow = %+v", follow)
		}
	})
	if decision := store.Authorize("a", clock); !decision.Allowed {
		t.Fatalf("decision = %+v, want the reset key admitted", decision)
	}
	clock = clock.Add(5 * time.Minute)
	if results := observe(restart.Add(time.Duration(testWeek)*time.Second), fiveHourEnd, 2); len(results) != 0 {
		t.Fatalf("the same reset was followed twice: %+v", results)
	}
	logs, err := store.PluginLogsPage(PluginLogQuery{Limit: 10, Levels: []PluginLogLevel{PluginLogInfo}})
	if err != nil || len(logs.Entries) == 0 || !strings.Contains(logs.Entries[0].Message, "cleared Weekly for 2 API keys") {
		t.Fatalf("logs = %+v, %v", logs.Entries, err)
	}
}

func TestSharedPlanFollowsUpstreamSchedule(t *testing.T) {
	now := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	clock := now
	store := followingStore(t, &clock, []QuotaWindow{{Name: "Weekly", AmountUSD: 10, PeriodSeconds: testWeek, CycleAnchorAt: now.Add(24 * time.Hour)}})
	var windowID string
	store.Read(func(state *State) { windowID = state.Plans[0].Windows[0].ID })
	var before QuotaCycle
	store.Read(func(state *State) { before = state.Keys["a"].Cycles[windowID] })

	// The first observation moves the schedule without discarding usage.
	clock = now.Add(time.Minute)
	upstreamEnd := now.Add(3*24*time.Hour + 17*time.Second)
	results := store.ObserveUpstream(testUpstreamCredential, clock, []UpstreamObservation{{PeriodSeconds: testWeek, ResetAt: upstreamEnd, UsedPercent: 40}})
	if len(results) != 1 || len(results[0].ResetWindows) != 0 || strings.Join(results[0].AlignedWindows, ",") != "Weekly" {
		t.Fatalf("results = %+v, want an alignment", results)
	}
	store.Read(func(state *State) {
		plan := state.Plans[0]
		cycle := state.Keys["a"].Cycles[windowID]
		if !plan.Windows[0].CycleAnchorAt.Equal(upstreamEnd) || !cycle.EndAt.Equal(upstreamEnd) ||
			cycle.SpentUSD != before.SpentUSD || cycle.SpentUSD == 0 || !cycle.UsageSince.Equal(before.UsageSince) {
			t.Fatalf("plan = %+v, cycle = %+v, before = %+v", plan.Windows, cycle, before)
		}
		if err := state.Keys["a"].ValidateCycles(plan); err != nil {
			t.Fatal(err)
		}
	})

	// A reset leaves Codex idle until its next request: usage clears at once,
	// and the schedule moves when the new window starts, keeping later usage.
	clock = now.Add(time.Hour)
	results = store.ObserveUpstream(testUpstreamCredential, clock, []UpstreamObservation{{PeriodSeconds: testWeek, ResetAt: clock.Add(time.Duration(testWeek) * time.Second), UsedPercent: 0}})
	if len(results) != 1 || results[0].ResetKeys != 2 || len(results[0].AlignedWindows) != 0 {
		t.Fatalf("results = %+v, want a reset without an alignment", results)
	}
	store.Read(func(state *State) {
		if len(state.Keys["a"].Cycles) != 0 || !state.Plans[0].Windows[0].CycleAnchorAt.Equal(upstreamEnd) {
			t.Fatalf("plan = %+v, cycles = %+v", state.Plans[0].Windows, state.Keys["a"].Cycles)
		}
	})
	store.RecordUsage(admittedEvent(store, "a", clock))
	clock = now.Add(time.Hour + 5*time.Minute)
	restartEnd := now.Add(time.Hour + 2*time.Minute + time.Duration(testWeek)*time.Second)
	results = store.ObserveUpstream(testUpstreamCredential, clock, []UpstreamObservation{{PeriodSeconds: testWeek, ResetAt: restartEnd, UsedPercent: 1}})
	if len(results) != 1 || results[0].ResetKeys != 0 || len(results[0].AlignedWindows) != 1 {
		t.Fatalf("results = %+v, want only an alignment", results)
	}
	store.Read(func(state *State) {
		cycle := state.Keys["a"].Cycles[windowID]
		if !state.Plans[0].Windows[0].CycleAnchorAt.Equal(restartEnd) || !cycle.EndAt.Equal(restartEnd) || cycle.SpentUSD == 0 {
			t.Fatalf("plan = %+v, cycle = %+v", state.Plans[0].Windows, cycle)
		}
	})
	decision := store.Authorize("b", clock)
	if !decision.Allowed || !decision.Windows[0].EndAt.Equal(restartEnd) {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestPlanPatchReplacesUpstreamFollowing(t *testing.T) {
	now := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	clock := now
	store := followingStore(t, &clock, []QuotaWindow{{Name: "Weekly", AmountUSD: 10, PeriodSeconds: testWeek}})
	store.ObserveUpstream(testUpstreamCredential, now.Add(time.Minute), []UpstreamObservation{{PeriodSeconds: testWeek, ResetAt: now.Add(time.Hour), UsedPercent: 1}})
	baseline := func() int {
		var count int
		store.Read(func(state *State) {
			if follow := state.Plans[0].UpstreamReset; follow != nil {
				count = len(follow.Windows)
			} else {
				count = -1
			}
		})
		return count
	}
	name := "Renamed"
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p", Name: &name}, nil); err != nil || baseline() != 1 {
		t.Fatalf("an unrelated edit changed following: %v, %d", err, baseline())
	}
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p", UpstreamReset: &UpstreamReset{Credential: testUpstreamCredential}}, nil); err != nil || baseline() != 1 {
		t.Fatalf("keeping the credential dropped its baseline: %v, %d", err, baseline())
	}
	other := CredentialFingerprint("other.json")
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p", UpstreamReset: &UpstreamReset{Credential: other}}, nil); err != nil || baseline() != 0 {
		t.Fatalf("a new credential kept the old baseline: %v, %d", err, baseline())
	}
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p", UpstreamReset: &UpstreamReset{Credential: "sha256:invalid"}}, nil); err == nil || KindOf(err) != KindInvalid {
		t.Fatalf("invalid credential error = %v", err)
	}
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p", UpstreamReset: &UpstreamReset{}}, nil); err != nil || baseline() != -1 {
		t.Fatalf("an empty credential did not stop following: %v, %d", err, baseline())
	}
	if credentials := store.UpstreamCredentials(""); len(credentials) != 0 {
		t.Fatalf("credentials = %v", credentials)
	}
}

func TestUpstreamCredentialsFollowTheKeyPlan(t *testing.T) {
	now := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	clock := now
	store := followingStore(t, &clock, []QuotaWindow{{Name: "Weekly", AmountUSD: 10, PeriodSeconds: testWeek}})
	for scope, want := range map[string]int{"": 1, "a": 1, "other": 0, "unknown": 0} {
		if got := store.UpstreamCredentials(scope); len(got) != want || want == 1 && got[0] != testUpstreamCredential {
			t.Fatalf("UpstreamCredentials(%q) = %v", scope, got)
		}
	}
}

// The admission that records the baseline opens the key's cycle a moment later.
// A restart reported as starting before that check still predates the reset
// only as far as the check can tell, so the cycle must be cleared.
func TestResetClearsTheCycleOpenedByTheBaselineCheck(t *testing.T) {
	now := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	clock := now
	store := newAccountStore(t, now)
	store.now = func() time.Time { return clock }
	store.ReplaceAll(func(state *State) { state.Keys["a"] = &KeyState{} })
	if _, err := store.CreatePlanWithBindings(Plan{ID: "p", Windows: []QuotaWindow{{Name: "Weekly", AmountUSD: 0.001, PeriodSeconds: testWeek}},
		UpstreamReset: &UpstreamReset{Credential: testUpstreamCredential}}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	store.ObserveUpstream(testUpstreamCredential, now, []UpstreamObservation{{PeriodSeconds: testWeek, ResetAt: now.Add(72 * time.Hour), UsedPercent: 90}})
	store.RecordUsage(admittedEvent(store, "a", now.Add(time.Microsecond)))
	clock = now.Add(time.Minute)
	if store.Authorize("a", clock).Allowed {
		t.Fatal("the weekly quota should be exhausted")
	}
	results := store.ObserveUpstream(testUpstreamCredential, clock, []UpstreamObservation{
		{PeriodSeconds: testWeek, ResetAt: now.Add(time.Duration(testWeek)*time.Second - 2*time.Minute), UsedPercent: 1},
	})
	if len(results) != 1 || results[0].ResetKeys != 1 {
		t.Fatalf("results = %+v, want the key reset", results)
	}
	if !store.Authorize("a", clock).Allowed {
		t.Fatal("the key stayed blocked after the upstream reset")
	}
}
