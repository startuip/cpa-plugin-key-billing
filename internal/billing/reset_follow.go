package billing

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"cpa-key-billing/internal/messages"
)

// Only ordinary provider windows with an explicit duration enter a snapshot.
// Percentages are deliberately absent: downstream accounting remains independent.
type UpstreamWindow struct {
	ID            string    `json:"id"`
	PeriodSeconds int64     `json:"period_seconds"`
	ResetAt       time.Time `json:"reset_at,omitzero"`
}

type ResetSnapshot struct {
	AuthIndex     string           `json:"auth_index"`
	Provider      string           `json:"provider"`
	CredentialRef string           `json:"credential_ref"`
	AttemptedAt   time.Time        `json:"attempted_at"`
	SyncedAt      time.Time        `json:"synced_at,omitzero"`
	Windows       []UpstreamWindow `json:"windows"`
	Error         ResetFollowError `json:"error,omitzero"`
}

type FollowWindow struct {
	UpstreamID    string    `json:"upstream_id"`
	PeriodSeconds int64     `json:"period_seconds"`
	NextResetAt   time.Time `json:"next_reset_at,omitzero"`
	LastResetAt   time.Time `json:"last_reset_at,omitzero"`
}

type ResetFollow struct {
	AuthIndex     string                  `json:"auth_index"`
	Provider      string                  `json:"provider"`
	CredentialRef string                  `json:"-"`
	Windows       map[string]FollowWindow `json:"windows"`
	AttemptedAt   time.Time               `json:"attempted_at,omitzero"`
	SyncedAt      time.Time               `json:"synced_at,omitzero"`
	Error         ResetFollowError        `json:"error,omitzero"`
}

// Persist the credential reference separately from public JSON views.
type StoredResetFollow struct {
	*ResetFollow
	CredentialRef string `json:"credential_ref"`
}

// The operation ID is scoped to the host account, never to an email or model.
type UpstreamReset struct {
	AuthIndex   string    `json:"auth_index"`
	ID          string    `json:"id"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	StartedAt   time.Time `json:"started_at"`
	Applied     bool      `json:"applied"`
}

func cloneResetFollow(f *ResetFollow) *ResetFollow {
	if f == nil {
		return nil
	}
	next := *f
	next.Windows = maps.Clone(f.Windows)
	return &next
}

func MatchResetWindows(plan Plan, snapshot ResetSnapshot, now time.Time) (map[string]FollowWindow, error) {
	if plan.ID == "" || len(plan.Windows) == 0 {
		return nil, invalidf("Reset following requires a subscription plan")
	}
	if snapshot.Provider != "codex" && snapshot.Provider != "claude" {
		return nil, invalidf("Reset following supports only Codex and Claude accounts")
	}
	if snapshot.Error.Text != "" {
		return nil, invalidf("Upstream reset synchronization failed: %s", snapshot.Error.Text)
	}
	matched := make(map[string]FollowWindow, len(plan.Windows))
	for _, window := range plan.Windows {
		var candidates []UpstreamWindow
		for _, upstream := range snapshot.Windows {
			if upstream.PeriodSeconds == window.PeriodSeconds {
				candidates = append(candidates, upstream)
			}
		}
		if len(candidates) != 1 {
			return nil, invalidf("Window %q (%d seconds) requires exactly one ordinary upstream window; found %d", window.Name, window.PeriodSeconds, len(candidates))
		}
		upstream := candidates[0]
		if upstream.ID == "" || !upstream.ResetAt.After(now) || upstream.ResetAt.Year() > 9999 {
			return nil, invalidf("Window %q has no valid future upstream reset time", window.Name)
		}
		matched[window.ID] = FollowWindow{UpstreamID: upstream.ID, PeriodSeconds: window.PeriodSeconds, NextResetAt: upstream.ResetAt}
	}
	return matched, nil
}

func followRouteAllowed(state *State, key *KeyState, snapshot ResetSnapshot) bool {
	decision := resolveRoutingState(state, key)
	return snapshot.CredentialRef != "" && decision.ConfigurationError == "" && decision.AllowsCredential(snapshot.CredentialRef, CredentialSourceAuthFiles, snapshot.Provider)
}

func (s *Store) SetResetFollow(scope, authIndex string) error {
	scope, authIndex = normalizeScope(scope), strings.TrimSpace(authIndex)
	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		key := state.liveKey(scope)
		if key == nil {
			return struct{}{}, Changes{}, notFoundf("API key %q does not exist", scope)
		}
		plan, _ := state.FindPlan(key.PlanID)
		now := s.Now()
		settleExpiredCycles(key, now)
		if authIndex == "" {
			if key.ResetFollow != nil {
				stopFollowing(key, plan, now)
			}
		} else {
			snapshot := state.ResetSnapshots[authIndex]
			matched, err := MatchResetWindows(plan, snapshot, now)
			if err != nil {
				return struct{}{}, Changes{}, err
			}
			if !followRouteAllowed(state, key, snapshot) {
				return struct{}{}, Changes{}, invalidf("The followed account must be allowed by this API key's routing rules")
			}
			previous := key.ResetFollow
			key.ResetFollow = &ResetFollow{AuthIndex: authIndex, Provider: snapshot.Provider, CredentialRef: snapshot.CredentialRef, Windows: matched, AttemptedAt: snapshot.AttemptedAt, SyncedAt: snapshot.SyncedAt}
			if previous != nil && previous.AuthIndex == authIndex {
				for id, window := range matched {
					window.LastResetAt = previous.Windows[id].LastResetAt
					matched[id] = window
				}
			}
			activateCycles(key, plan, now)
			for id, window := range matched {
				cycle := key.Cycles[id]
				cycle.EndAt = window.NextResetAt
				key.Cycles[id] = cycle
			}
		}
		return struct{}{}, Changes{Keys: []string{scope}}, nil
	})
	return err
}

// Called inside the configuration transaction. Route/plan edits cannot silently
// leave a following key incompatible, and failed edits keep all previous
// settings. Edits never need a fresh upstream snapshot, and deleted keys, which
// cannot be managed, stop following instead of blocking an edit. Returns the
// keys it changed so they are saved with the edit.
func validateFollowConfiguration(previous, next *State, now time.Time) ([]string, error) {
	var changed []string
	for scope, key := range next.Keys {
		if key == nil || key.ResetFollow == nil {
			continue
		}
		plan, _ := next.FindPlan(key.PlanID)
		if followUnchanged(previous, next, previous.Keys[scope], key, plan) {
			continue
		}
		err := refollow(next, key, plan, now)
		if err != nil && !key.DeletedAt.IsZero() {
			stopFollowing(key, plan, now)
			err = nil
		}
		if err == nil {
			// Loading rejects inconsistent cycles; never save what cannot be loaded.
			err = key.ValidateCycles(plan)
		}
		if err != nil {
			return nil, err
		}
		changed = append(changed, scope)
	}
	return changed, nil
}

// Names and limits do not move upstream boundaries; only the followed account,
// window schedules (a changed one drops its cycle), the cycle set and credential
// routing matter.
func followUnchanged(previous, next *State, old, key *KeyState, plan Plan) bool {
	if old == nil || old.ResetFollow == nil || old.ResetFollow.AuthIndex != key.ResetFollow.AuthIndex ||
		old.PlanID != key.PlanID || plan.ID == "" || len(key.Cycles) != len(plan.Windows) {
		return false
	}
	oldPlan, _ := previous.FindPlan(old.PlanID)
	if len(oldPlan.Windows) != len(plan.Windows) {
		return false
	}
	for _, window := range plan.Windows {
		index := slices.IndexFunc(oldPlan.Windows, func(other QuotaWindow) bool { return other.ID == window.ID })
		if index < 0 || !window.sameSchedule(oldPlan.Windows[index]) {
			return false
		}
		if _, ok := key.Cycles[window.ID]; !ok {
			return false
		}
	}
	return reflect.DeepEqual(resolveRoutingState(next, key), resolveRoutingState(previous, old))
}

// refollow re-derives a following key's windows after a plan or routing edit.
// A window whose ID and period are unchanged keeps its known boundaries. A new
// or rescheduled window takes the last known upstream window of its period; an
// expired reset time stays unknown until the next synchronization.
func refollow(state *State, key *KeyState, plan Plan, now time.Time) error {
	name := followKeyName(key)
	if plan.ID == "" {
		return invalidf("API key %q follows upstream resets; turn off following before removing its subscription plan", name)
	}
	if key.Cycles == nil {
		key.Cycles = make(map[string]QuotaCycle)
	}
	// Consume confirmed boundaries first, so a kept window never carries an
	// expired reset time into a recreated cycle.
	settleFollowCycles(key, now)
	snapshot := state.ResetSnapshots[key.ResetFollow.AuthIndex]
	windows := make(map[string]FollowWindow, len(plan.Windows))
	for _, window := range plan.Windows {
		if current, ok := key.ResetFollow.Windows[window.ID]; ok && current.PeriodSeconds == window.PeriodSeconds {
			windows[window.ID] = current
			continue
		}
		var candidates []UpstreamWindow
		for _, upstream := range snapshot.Windows {
			if upstream.PeriodSeconds == window.PeriodSeconds && upstream.ID != "" {
				candidates = append(candidates, upstream)
			}
		}
		if len(candidates) != 1 {
			return invalidf("API key %q: window %q (%d seconds) needs exactly one ordinary upstream window; found %d",
				name, window.Name, window.PeriodSeconds, len(candidates))
		}
		matched := FollowWindow{UpstreamID: candidates[0].ID, PeriodSeconds: window.PeriodSeconds}
		if resetAt := candidates[0].ResetAt; resetAt.After(now) && resetAt.Year() <= 9999 {
			matched.NextResetAt = resetAt
		}
		windows[window.ID] = matched
	}
	key.ResetFollow.Windows = windows
	for id := range key.Cycles {
		if _, ok := windows[id]; !ok {
			delete(key.Cycles, id)
		}
	}
	activateCycles(key, plan, now)
	for id, window := range windows {
		cycle := key.Cycles[id]
		cycle.EndAt = window.NextResetAt
		key.Cycles[id] = cycle
	}
	decision := resolveRoutingState(state, key)
	if decision.ConfigurationError != "" || !decision.AllowsCredential(key.ResetFollow.CredentialRef, CredentialSourceAuthFiles, key.ResetFollow.Provider) {
		return invalidf("API key %q: the followed account must be allowed by its routing rules", name)
	}
	return nil
}

// stopFollowing keeps the current counters for the rest of one native period
// from each cycle's start. Later cycles use the plan's own schedule.
func stopFollowing(key *KeyState, plan Plan, now time.Time) {
	if plan.ID == "" {
		key.Cycles, key.ResetFollow = nil, nil
		return
	}
	if key.Cycles == nil {
		key.Cycles = make(map[string]QuotaCycle)
	}
	settleFollowCycles(key, now)
	for id := range key.Cycles {
		if !slices.ContainsFunc(plan.Windows, func(window QuotaWindow) bool { return window.ID == id }) {
			delete(key.Cycles, id)
		}
	}
	for _, window := range plan.Windows {
		cycle, ok := key.Cycles[window.ID]
		if !ok {
			continue
		}
		// Counters last one native period from the cycle's start, never longer;
		// a cycle already past that starts afresh at the next admission.
		end := window.newCycle(plan.ID, cycle.StartAt).EndAt
		if !end.After(now) {
			delete(key.Cycles, window.ID)
			continue
		}
		cycle.EndAt, cycle.ScheduleOverride = end, true
		key.Cycles[window.ID] = cycle
	}
	key.ResetFollow = nil
}

func followKeyName(key *KeyState) string {
	if label := strings.TrimSpace(key.Label); label != "" {
		return label
	}
	return key.Preview
}

// A known boundary is consumed exactly once. An unknown next boundary keeps the
// current counters indefinitely, including when admission is already blocked.
func settleFollowCycles(key *KeyState, now time.Time) bool {
	changed := false
	for id, window := range key.ResetFollow.Windows {
		if window.NextResetAt.IsZero() || now.Before(window.NextResetAt) {
			continue
		}
		boundary := window.NextResetAt
		key.Cycles[id] = QuotaCycle{PlanID: key.PlanID, StartAt: boundary, UsageSince: boundary}
		window.LastResetAt, window.NextResetAt = boundary, time.Time{}
		key.ResetFollow.Windows[id] = window
		changed = true
	}
	return changed
}

func (s *Store) FollowedAccounts() []string {
	var accounts []string
	s.read(func(state *State) {
		for _, key := range state.Keys {
			if key != nil && key.DeletedAt.IsZero() && key.ResetFollow != nil {
				accounts = append(accounts, key.ResetFollow.AuthIndex)
			}
		}
	})
	slices.Sort(accounts)
	return slices.Compact(accounts)
}

// Account operations are serialized by the plugin. Applying a snapshot consults
// the current configuration, never the key set captured before the network call.
func (s *Store) ApplyResetSnapshot(snapshot ResetSnapshot) {
	snapshot.Windows = slices.Clone(snapshot.Windows)
	updateResult(s, func(state *State) (struct{}, Changes) {
		previous := state.ResetSnapshots[snapshot.AuthIndex]
		if snapshot.AttemptedAt.Before(previous.AttemptedAt) {
			return struct{}{}, Changes{}
		}
		if snapshot.Error.Text != "" {
			snapshot.Windows, snapshot.SyncedAt = previous.Windows, previous.SyncedAt
			snapshot.Provider, snapshot.CredentialRef = previous.Provider, previous.CredentialRef
		}
		state.ResetSnapshots[snapshot.AuthIndex] = snapshot
		var scopes []string
		for scope, key := range state.Keys {
			if key == nil || key.ResetFollow == nil || key.ResetFollow.AuthIndex != snapshot.AuthIndex {
				continue
			}
			settleFollowCycles(key, s.Now())
			follow := key.ResetFollow
			follow.AttemptedAt, follow.Error = snapshot.AttemptedAt, snapshot.Error
			plan, _ := state.FindPlan(key.PlanID)
			matched, err := MatchResetWindows(plan, snapshot, s.Now())
			if err == nil && !followRouteAllowed(state, key, snapshot) {
				err = invalidf("The followed account must be allowed by this API key's routing rules")
			}
			if err != nil {
				// Preserve the query's translation metadata instead of wrapping its
				// already-rendered English text as an untranslatable parameter.
				if snapshot.Error.Text == "" {
					follow.Error = ResetError(messages.FromError(err))
				}
			} else {
				follow.SyncedAt = snapshot.SyncedAt
				follow.Provider, follow.CredentialRef = snapshot.Provider, snapshot.CredentialRef
				for id, window := range matched {
					old := follow.Windows[id]
					// A repeated or obsolete boundary never clears usage or becomes armed again.
					if !window.NextResetAt.After(old.LastResetAt) {
						continue
					}
					window.LastResetAt = old.LastResetAt
					follow.Windows[id] = window
					cycle := key.Cycles[id]
					cycle.EndAt = window.NextResetAt
					key.Cycles[id] = cycle
				}
			}
			scopes = append(scopes, scope)
		}
		return struct{}{}, Changes{Keys: scopes, ResetSnapshots: []string{snapshot.AuthIndex}}
	})
}

func resetOperationKey(authIndex, id string) string { return authIndex + ":" + id }

// Operations only deduplicate retries of one reset request, which the provider
// also deduplicates by ID, so a week of history is ample.
const upstreamResetRetention = 7 * 24 * time.Hour

// Save the ID before contacting the provider, so retries reuse the same operation.
func (s *Store) BeginUpstreamReset(authIndex, id string) (UpstreamReset, error) {
	return editConfiguration(s, func(state *State) (UpstreamReset, Changes, error) {
		now := s.Now()
		var changed []string
		for operationKey, operation := range state.UpstreamResets {
			if now.Sub(operation.StartedAt) > upstreamResetRetention {
				delete(state.UpstreamResets, operationKey)
				changed = append(changed, operationKey)
			}
		}
		operationKey := resetOperationKey(authIndex, id)
		if operation, exists := state.UpstreamResets[operationKey]; exists {
			return operation, Changes{UpstreamResets: changed}, nil
		}
		operation := UpstreamReset{AuthIndex: authIndex, ID: id, StartedAt: now}
		state.UpstreamResets[operationKey] = operation
		return operation, Changes{UpstreamResets: append(changed, operationKey)}, nil
	})
}

// AbandonUpstreamReset forgets an operation the provider did not confirm. A
// retry with the same ID starts over, and the provider deduplicates it.
func (s *Store) AbandonUpstreamReset(operation UpstreamReset) {
	updateResult(s, func(state *State) (struct{}, Changes) {
		operationKey := resetOperationKey(operation.AuthIndex, operation.ID)
		if stored, exists := state.UpstreamResets[operationKey]; !exists || stored.Applied {
			return struct{}{}, Changes{}
		}
		delete(state.UpstreamResets, operationKey)
		return struct{}{}, Changes{UpstreamResets: []string{operationKey}}
	})
}

// UpstreamResetApplied reports whether this reset request already succeeded.
func (s *Store) UpstreamResetApplied(authIndex, id string) bool {
	applied := false
	s.read(func(state *State) { applied = state.UpstreamResets[resetOperationKey(authIndex, id)].Applied })
	return applied
}

// Usage and reset effects stay live if a write fails, and are retried together.
func (s *Store) ApplyUpstreamReset(operation UpstreamReset) {
	updateResult(s, func(state *State) (struct{}, Changes) {
		operationKey := resetOperationKey(operation.AuthIndex, operation.ID)
		stored, exists := state.UpstreamResets[operationKey]
		if !exists || stored.Applied {
			return struct{}{}, Changes{}
		}
		stored.Applied = true
		stored.CompletedAt = s.Now()
		state.UpstreamResets[operationKey] = stored
		snapshot := state.ResetSnapshots[operation.AuthIndex]
		snapshot.AuthIndex = operation.AuthIndex
		snapshot.Windows = nil
		snapshot.Error = ResetError(messages.New("Waiting for upstream reset times"))
		state.ResetSnapshots[operation.AuthIndex] = snapshot
		var scopes []string
		for scope, key := range state.Keys {
			if key == nil || key.ResetFollow == nil || key.ResetFollow.AuthIndex != operation.AuthIndex {
				continue
			}
			for id, window := range key.ResetFollow.Windows {
				key.Cycles[id] = QuotaCycle{PlanID: key.PlanID, StartAt: stored.CompletedAt, UsageSince: stored.CompletedAt}
				window.LastResetAt, window.NextResetAt = stored.CompletedAt, time.Time{}
				key.ResetFollow.Windows[id] = window
			}
			key.ResetFollow.Error = snapshot.Error
			scopes = append(scopes, scope)
		}
		return struct{}{}, Changes{Keys: scopes, ResetSnapshots: []string{operation.AuthIndex}, UpstreamResets: []string{operationKey}}
	})
}

// Preserve raw diagnostics as well as translation metadata across restarts.
type ResetFollowError struct {
	messages.Message
	Text string `json:"message"`
}

func (e ResetFollowError) IsZero() bool { return e.Text == "" }
func ResetError(message messages.Message) ResetFollowError {
	return ResetFollowError{Message: message, Text: message.Text}
}

func (key *KeyState) validateResetFollow(plan Plan) error {
	follow := key.ResetFollow
	if follow == nil {
		return nil
	}
	if plan.ID == "" || follow.AuthIndex == "" || follow.CredentialRef == "" ||
		(follow.Provider != "codex" && follow.Provider != "claude") ||
		len(follow.Windows) != len(plan.Windows) || len(key.Cycles) != len(plan.Windows) {
		return invalidf("Invalid quota cycle data for this API key")
	}
	for _, window := range plan.Windows {
		matched, ok := follow.Windows[window.ID]
		if !ok || matched.UpstreamID == "" || matched.PeriodSeconds != window.PeriodSeconds ||
			!key.Cycles[window.ID].EndAt.Equal(matched.NextResetAt) ||
			!matched.NextResetAt.IsZero() && !matched.NextResetAt.After(matched.LastResetAt) {
			return invalidf("Invalid quota cycle data for this API key")
		}
	}
	return nil
}
