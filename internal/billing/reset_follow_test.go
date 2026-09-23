package billing

import (
	"errors"
	"reflect"
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
