package billing

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-key-billing/internal/messages"
)

func followStore(t *testing.T) (*Store, *memoryRepository, *time.Time, ResetSnapshot) {
	t.Helper()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store, repo := newAccountStoreWithRepository(t, now)
	store.now = func() time.Time { return now }
	store.ReplaceAll(func(state *State) {
		state.Plans = []Plan{{ID: "p", Windows: []QuotaWindow{
			{ID: "short", Name: "Short", PeriodSeconds: 18000, AmountUSD: 1, TokenLimit: 6000, RequestLimit: 3},
			{ID: "week", Name: "Week", PeriodSeconds: 604800, AmountUSD: 10, TokenLimit: 60000, RequestLimit: 30},
		}}}
		for _, scope := range []string{"a", "b"} {
			state.Keys[scope] = &KeyState{Preview: "sk-dum…0001", PlanID: "p"}
		}
	})
	snapshot := ResetSnapshot{AuthIndex: "dummy-auth", Provider: "codex", CredentialRef: CredentialFingerprint("dummy-auth-id"), AttemptedAt: now, SyncedAt: now,
		Windows: []UpstreamWindow{{ID: "primary", PeriodSeconds: 18000, ResetAt: now.Add(time.Hour)}, {ID: "secondary", PeriodSeconds: 604800, ResetAt: now.Add(24 * time.Hour)}}}
	store.ApplyResetSnapshot(snapshot)
	for _, scope := range []string{"a", "b"} {
		if err := store.SetResetFollow(scope, snapshot.AuthIndex); err != nil {
			t.Fatal(err)
		}
	}
	return store, repo, &now, snapshot
}

func followCycle(t *testing.T, store *Store, scope, id string) QuotaCycle {
	t.Helper()
	var cycle QuotaCycle
	store.Read(func(state *State) { cycle = state.Keys[scope].Cycles[id] })
	return cycle
}

func TestMatchResetWindows(t *testing.T) {
	store, _, now, snapshot := followStore(t)
	plan := store.Plans()[0]
	for _, tc := range []struct {
		name   string
		change func(*ResetSnapshot)
	}{
		{"missing", func(s *ResetSnapshot) { s.Windows = s.Windows[:1] }},
		{"ambiguous", func(s *ResetSnapshot) { s.Windows = append(s.Windows, s.Windows[0]) }},
		{"unknown duration", func(s *ResetSnapshot) { s.Windows[0].PeriodSeconds = 0 }},
		{"past boundary", func(s *ResetSnapshot) { s.Windows[0].ResetAt = *now }},
		{"no boundary", func(s *ResetSnapshot) { s.Windows[0].ResetAt = time.Time{} }},
		{"unsupported", func(s *ResetSnapshot) { s.Provider = "kimi" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := snapshot
			current.Windows = append([]UpstreamWindow(nil), snapshot.Windows...)
			tc.change(&current)
			if _, err := MatchResetWindows(plan, current, *now); err == nil {
				t.Fatal("invalid match accepted")
			}
		})
	}
	snapshot.Provider = "claude"
	if _, err := MatchResetWindows(plan, snapshot, *now); err != nil {
		t.Fatal(err)
	}
}

func TestFollowBoundariesIndependentUsageAndLateRecords(t *testing.T) {
	store, _, now, _ := followStore(t)
	initial := *now
	for range 3 {
		store.RecordUsage(subsetEvent("a", *now))
	}
	if store.Authorize("a", *now).Allowed || !store.Authorize("b", *now).Allowed {
		t.Fatal("keys must account independently")
	}
	if got := followCycle(t, store, "a", "short"); got.UsedTokens != 4500 || got.UsedRequests != 3 || got.SpentUSD <= 0 {
		t.Fatalf("lost dimensions: %+v", got)
	}
	*now = now.Add(time.Hour)
	// No scheduler activity: a quota read executes the confirmed boundary.
	view, _ := store.KeyViewForScope("a")
	if view.Blocked || !view.Windows[0].EndAt.IsZero() {
		t.Fatalf("boundary not consumed: %+v", view)
	}
	late := subsetEvent("a", initial)
	late.At = *now
	store.RecordUsage(late)
	store.RecordUsage(subsetEvent("a", *now))
	for range 4 {
		store.KeyViews()
		store.Authorize("a", *now)
	}
	if got := followCycle(t, store, "a", "short"); got.UsedRequests != 1 {
		t.Fatalf("duplicate reset or late charge: %+v", got)
	}
	if got := followCycle(t, store, "a", "week"); got.UsedRequests != 5 {
		t.Fatalf("unrelated window reset: %+v", got)
	}
	if got := followCycle(t, store, "b", "short"); got.UsedRequests != 0 {
		t.Fatalf("shared usage: %+v", got)
	}
}

func TestFollowFailureWaitsAndRecoveryPreservesUsage(t *testing.T) {
	store, _, now, snapshot := followStore(t)
	for range 3 {
		store.RecordUsage(subsetEvent("a", *now))
	}
	failure := ResetSnapshot{AuthIndex: snapshot.AuthIndex, AttemptedAt: now.Add(time.Minute), Error: ResetError(messages.Literal("dummy network failure"))}
	store.ApplyResetSnapshot(failure)
	if store.Authorize("a", *now).Allowed {
		t.Fatal("query failure unblocked exhausted key")
	}
	*now = now.Add(time.Hour)
	if !store.Authorize("a", *now).Allowed {
		t.Fatal("known boundary did not run during failure")
	}
	for range 3 {
		store.RecordUsage(subsetEvent("a", *now))
	}
	*now = now.Add(6 * time.Hour)
	if store.Authorize("a", *now).Allowed {
		t.Fatal("unknown boundary was extrapolated")
	}
	snapshot.AttemptedAt, snapshot.SyncedAt = *now, *now
	snapshot.Windows[0].ResetAt = now.Add(time.Hour)
	store.ApplyResetSnapshot(snapshot)
	if store.Authorize("a", *now).Allowed {
		t.Fatal("recovery cleared waiting usage")
	}
	// A different reset_at is an update, not a reset operation.
	snapshot.Windows[0].ResetAt = now.Add(2 * time.Hour)
	store.ApplyResetSnapshot(snapshot)
	if got := followCycle(t, store, "a", "short"); got.UsedRequests != 3 {
		t.Fatalf("changed timestamp cleared usage: %+v", got)
	}
	*now = now.Add(2 * time.Hour)
	if !store.Authorize("a", *now).Allowed {
		t.Fatal("new confirmed boundary did not reset")
	}
}

func TestFollowEnableSwitchDisablePreservesUsage(t *testing.T) {
	store, _, now, snapshot := followStore(t)
	store.RecordUsage(subsetEvent("a", *now))
	for _, index := range []string{"", snapshot.AuthIndex, "dummy-other", ""} {
		if index == "dummy-other" {
			snapshot.AuthIndex = index
			store.ApplyResetSnapshot(snapshot)
		}
		if err := store.SetResetFollow("a", index); err != nil {
			t.Fatal(err)
		}
		if got := followCycle(t, store, "a", "short"); got.UsedRequests != 1 || got.UsedTokens != 1500 {
			t.Fatalf("toggle changed usage: %+v", got)
		}
		store.Read(func(state *State) {
			if err := state.Keys["a"].ValidateCycles(state.Plans[0]); err != nil {
				t.Fatal(err)
			}
		})
	}
	before, _ := store.KeyViewForScope("b")
	snapshot.AuthIndex = "invalid"
	snapshot.Windows = snapshot.Windows[:1]
	store.ApplyResetSnapshot(snapshot)
	if err := store.SetResetFollow("b", "invalid"); err == nil {
		t.Fatal("incompatible account accepted")
	}
	after, _ := store.KeyViewForScope("b")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed edit changed key")
	}
}

func TestFollowConfigurationEditsAreAtomic(t *testing.T) {
	store, _, _, _ := followStore(t)
	old := store.Plans()[0]
	windows := append([]QuotaWindow(nil), old.Windows...)
	windows[0].PeriodSeconds = 3600
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: old.ID, Windows: &windows}, nil); err == nil {
		t.Fatal("incompatible period accepted")
	}
	if !reflect.DeepEqual(store.Plans()[0], old) {
		t.Fatal("failed plan edit was published")
	}
	if err := store.UnbindKey("a"); err == nil {
		t.Fatal("unbound following key")
	}
	if _, err := store.DeletePlan("p"); err == nil {
		t.Fatal("deleted followed plan")
	}
	rule := RouteBindings{RouteRule: RouteRule{DeniedCredentialProviders: []CredentialProviderSelector{{Source: CredentialSourceAuthFiles, Provider: "codex"}}}}
	if err := store.SetKeyRoutes("a", rule); err == nil {
		t.Fatal("excluded followed credential")
	}
	route, err := store.CreateRoute(Route{Name: "allowed"}, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	denied := rule.RouteRule
	if _, err := store.UpdateRoute(RoutePatch{ID: route.ID, Rule: &denied}, nil); err == nil {
		t.Fatal("shared route edit excluded follower")
	}
	windows = append([]QuotaWindow(nil), old.Windows...)
	windows[0].RequestLimit = 4
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: old.ID, Windows: &windows}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFollowManualResetIdempotenceAndWriteRecovery(t *testing.T) {
	store, repo, now, snapshot := followStore(t)
	initial := *now
	for _, scope := range []string{"a", "b"} {
		store.RecordUsage(subsetEvent(scope, *now))
	}
	operation, err := store.BeginUpstreamReset(snapshot.AuthIndex, "dummy-operation-id")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	repo.fail = errors.New("dummy disk failure")
	store.ApplyUpstreamReset(operation)
	for _, scope := range []string{"a", "b"} {
		event := subsetEvent(scope, initial)
		event.At = *now
		store.RecordUsage(event)
		store.RecordUsage(subsetEvent(scope, *now))
		if got := followCycle(t, store, scope, "short"); got.UsedRequests != 1 {
			t.Fatalf("late usage charged after manual reset: %+v", got)
		}
	}
	store.ApplyUpstreamReset(operation)
	if got := followCycle(t, store, "a", "short"); got.UsedRequests != 1 {
		t.Fatal("duplicate reset changed usage")
	}
	repo.fail = nil
	repeated, err := store.BeginUpstreamReset(snapshot.AuthIndex, operation.ID)
	if err != nil || !repeated.Applied {
		t.Fatalf("operation not retained: %+v %v", repeated, err)
	}
	if !repo.state.UpstreamResets[resetOperationKey(snapshot.AuthIndex, operation.ID)].Applied {
		t.Fatal("operation not retried with usage")
	}
}

func TestFollowConcurrentBoundaryAndUsage(t *testing.T) {
	store, _, now, _ := followStore(t)
	*now = now.Add(time.Hour)
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			store.Authorize("a", *now)
			store.RecordUsage(subsetEvent("a", *now))
			store.KeyViews()
		}()
	}
	group.Wait()
	if got := followCycle(t, store, "a", "short"); got.UsedRequests != 32 {
		t.Fatalf("concurrent boundary lost usage: %+v", got)
	}
}

func TestFollowPlanBindingRevalidatesAllWindows(t *testing.T) {
	store, _, _, _ := followStore(t)
	compatible, err := store.CreatePlanWithBindings(Plan{Name: "Compatible", Windows: []QuotaWindow{{Name: "5 hours", PeriodSeconds: 18000, RequestLimit: 10}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	incompatible, err := store.CreatePlanWithBindings(Plan{Name: "Incompatible", Windows: []QuotaWindow{{Name: "Daily", PeriodSeconds: 86400, RequestLimit: 10}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindKey("a", incompatible.ID); err == nil {
		t.Fatal("incompatible binding accepted")
	}
	if err := store.BindKey("a", compatible.ID); err != nil {
		t.Fatal(err)
	}
	view, _ := store.KeyViewForScope("a")
	if view.PlanID != compatible.ID || len(view.ResetFollow.Windows) != 1 {
		t.Fatalf("compatible binding lost follow state: %+v", view)
	}
	store.Read(func(state *State) {
		if err := state.Keys["a"].ValidateCycles(compatible); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFollowLocalResetConsumesExpiredBoundaryBeforeExcludingLateUsage(t *testing.T) {
	store, _, now, _ := followStore(t)
	*now = now.Add(time.Hour + time.Minute)
	if _, err := store.ResetQuota(ResetRequest{Mode: "all", Scopes: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	late := subsetEvent("a", now.Add(-30*time.Second))
	late.At = *now
	store.RecordUsage(late)
	if got := followCycle(t, store, "a", "short"); got.UsedRequests != 0 || !got.UsageSince.Equal(*now) {
		t.Fatalf("late usage escaped local reset: %+v", got)
	}
}

func TestFollowEditsDoNotNeedFreshSnapshot(t *testing.T) {
	store, _, now, snapshot := followStore(t)
	if err := store.SetResetFollow("b", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetLabel("a", "Team A"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		store.RecordUsage(subsetEvent("a", *now))
	}
	// Both upstream reset times pass without another synchronization.
	*now = now.Add(25 * time.Hour)
	name := "Renamed"
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p", Name: &name}, nil); err != nil {
		t.Fatalf("rename required a fresh snapshot: %v", err)
	}
	allowed := RouteBindings{RouteRule: RouteRule{CredentialProviders: []CredentialProviderSelector{{Source: CredentialSourceAuthFiles, Provider: "codex"}}}}
	if err := store.SetKeyRoutes("a", allowed); err != nil {
		t.Fatalf("compatible route edit required a fresh snapshot: %v", err)
	}
	plan := store.Plans()[0]
	// Recreate the weekly window: the short window keeps its boundary, and the
	// new one takes the last known weekly upstream window with an unknown reset.
	windows := []QuotaWindow{plan.Windows[0], {Name: "New week", PeriodSeconds: 604800, RequestLimit: 30}}
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p", Windows: &windows}, nil); err != nil {
		t.Fatalf("rescheduled window required a fresh snapshot: %v", err)
	}
	plan = store.Plans()[0]
	store.Read(func(state *State) {
		key := state.Keys["a"]
		if err := key.ValidateCycles(plan); err != nil {
			t.Fatal(err)
		}
		short, week := key.ResetFollow.Windows["short"], key.ResetFollow.Windows[plan.Windows[1].ID]
		if !short.LastResetAt.Equal(snapshot.Windows[0].ResetAt) || !short.NextResetAt.IsZero() || key.Cycles["short"].UsedRequests != 0 {
			t.Fatalf("expired boundary was not consumed exactly once: %+v %+v", short, key.Cycles["short"])
		}
		if week.UpstreamID != "secondary" || !week.NextResetAt.IsZero() {
			t.Fatalf("new window = %+v", week)
		}
	})
	snapshot.AttemptedAt, snapshot.SyncedAt = *now, *now
	snapshot.Windows[0].ResetAt, snapshot.Windows[1].ResetAt = now.Add(time.Hour), now.Add(48*time.Hour)
	store.ApplyResetSnapshot(snapshot)
	view, _ := store.KeyViewForScope("a")
	if !view.Windows[0].EndAt.Equal(now.Add(time.Hour)) || !view.Windows[1].EndAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("synchronization did not arm the edited windows: %+v", view.Windows)
	}
	windows = append([]QuotaWindow(nil), plan.Windows...)
	windows[1].PeriodSeconds = 86400
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p", Windows: &windows}, nil); err == nil || !strings.Contains(err.Error(), `"Team A"`) {
		t.Fatalf("unmatched window error = %v", err)
	}
	if err := store.UnbindKey("a"); err == nil || !strings.Contains(err.Error(), `"Team A"`) {
		t.Fatalf("unbind error = %v", err)
	}
	denied := RouteBindings{RouteRule: RouteRule{DeniedCredentialProviders: allowed.CredentialProviders}}
	if err := store.SetKeyRoutes("a", denied); err == nil || !strings.Contains(err.Error(), `"Team A"`) {
		t.Fatalf("route error = %v", err)
	}
}

func TestFollowDeletedKeyStopsFollowingInsteadOfBlockingEdits(t *testing.T) {
	store, _, now, _ := followStore(t)
	if err := store.SetResetFollow("b", ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store.RecordUsage(subsetEvent("a", *now))
	}
	store.ReplaceAll(func(state *State) { state.Keys["a"].DeletedAt = *now })
	*now = now.Add(2 * time.Hour)
	if accounts := store.FollowedAccounts(); len(accounts) != 0 {
		t.Fatalf("deleted key is still synchronized: %v", accounts)
	}
	name := "Renamed"
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p", Name: &name}, nil); err != nil {
		t.Fatalf("rename blocked by a deleted key: %v", err)
	}
	plan := store.Plans()[0]
	windows := append([]QuotaWindow(nil), plan.Windows...)
	windows[1].PeriodSeconds = 86400
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: "p", Windows: &windows}, nil); err != nil {
		t.Fatalf("schedule edit blocked by a deleted key: %v", err)
	}
	plan = store.Plans()[0]
	store.Read(func(state *State) {
		key := state.Keys["a"]
		if key.ResetFollow != nil {
			t.Fatal("incompatible deleted key kept following")
		}
		if err := key.ValidateCycles(plan); err != nil {
			t.Fatal(err)
		}
		// The cycle restarted at the expired boundary an hour ago and keeps one native period.
		if cycle := key.Cycles["short"]; !cycle.ScheduleOverride || cycle.UsedRequests != 0 || !cycle.EndAt.Equal(now.Add(4*time.Hour)) {
			t.Fatalf("expired boundary or native schedule lost: %+v", cycle)
		}
	})
	if _, err := store.DeletePlan("p"); err != nil {
		t.Fatalf("plan deletion blocked by a deleted key: %v", err)
	}
	store.Read(func(state *State) {
		if key := state.Keys["a"]; key.PlanID != "" || key.ResetFollow != nil || key.Cycles != nil {
			t.Fatalf("deleted key kept a detached plan: %+v", key)
		}
	})
}

func TestStopFollowingKeepsAtMostOneNativePeriod(t *testing.T) {
	store, _, now, _ := followStore(t)
	start := *now
	store.RecordUsage(subsetEvent("a", *now))
	// The short upstream boundary resets the short cycle an hour later.
	*now = start.Add(3 * time.Hour)
	store.RecordUsage(subsetEvent("a", *now))
	if err := store.SetResetFollow("a", ""); err != nil {
		t.Fatal(err)
	}
	short, week := followCycle(t, store, "a", "short"), followCycle(t, store, "a", "week")
	if !short.EndAt.Equal(start.Add(6*time.Hour)) || short.UsedRequests != 1 {
		t.Fatalf("short cycle outlived its period: %+v", short)
	}
	if !week.EndAt.Equal(start.Add(7*24*time.Hour)) || week.UsedRequests != 2 {
		t.Fatalf("weekly cycle outlived its period: %+v", week)
	}

	// Awaiting synchronization, the short cycle has run past its native period.
	store, _, now, _ = followStore(t)
	start = *now
	store.RecordUsage(subsetEvent("b", *now))
	*now = start.Add(9 * time.Hour)
	store.RecordUsage(subsetEvent("b", *now))
	if err := store.SetResetFollow("b", ""); err != nil {
		t.Fatal(err)
	}
	store.Read(func(state *State) {
		key := state.Keys["b"]
		if _, kept := key.Cycles["short"]; kept {
			t.Fatalf("a cycle older than its period was kept: %+v", key.Cycles["short"])
		}
		if err := key.ValidateCycles(state.Plans[0]); err != nil {
			t.Fatal(err)
		}
	})
	if !store.Authorize("b", *now).Allowed || followCycle(t, store, "b", "short").UsedRequests != 0 {
		t.Fatal("the next admission did not start a fresh native cycle")
	}
}
