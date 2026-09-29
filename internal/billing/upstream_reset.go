package billing

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"
)

// UpstreamReset makes a plan follow the rate-limit windows of one Codex auth
// file. Plan windows follow the upstream window with the same period: an early
// upstream reset clears their usage, and shared schedules keep the upstream
// reset time.
//
// Codex reports only each window's current end and usage, never a reset event,
// so Windows keeps the last observation as the baseline for recognizing one.
type UpstreamReset struct {
	// Credential is the CredentialFingerprint of the followed auth file.
	Credential  string           `json:"credential"`
	Windows     []UpstreamWindow `json:"windows,omitempty"`
	LastResetAt time.Time        `json:"last_reset_at,omitzero"`
}

// UpstreamWindow is a started upstream window as last observed. UsedPercent
// and ObservedAt are refreshed in memory on every observation and persisted
// with the next plan write.
type UpstreamWindow struct {
	PeriodSeconds int64     `json:"period_seconds"`
	ResetAt       time.Time `json:"reset_at"`
	UsedPercent   float64   `json:"used_percent"`
	ObservedAt    time.Time `json:"observed_at"`
}

// UpstreamObservation is one upstream window as currently reported. ResetAt
// uses the plugin's clock.
type UpstreamObservation struct {
	PeriodSeconds int64
	ResetAt       time.Time
	UsedPercent   float64
}

type UpstreamFollowResult struct {
	PlanID string `json:"plan_id"`
	// ResetWindows names the plan windows whose usage was cleared.
	ResetWindows []string `json:"reset_windows,omitempty"`
	ResetKeys    int      `json:"reset_keys"`
	// AlignedWindows names the shared-schedule windows moved to the upstream reset time.
	AlignedWindows []string `json:"aligned_windows,omitempty"`
}

const (
	// upstreamResetTolerance absorbs rounding and request latency in reported
	// reset times, and bounds clock skew between the plugin and upstream.
	upstreamResetTolerance = time.Minute
	// Within one window, usage must fall by at least this many percentage
	// points, to at most a tenth, to count as a reset. Raised limits lower the
	// percentage too, but not to a tenth of a meaningful value.
	upstreamUsageDropPoints = 5
)

func normalizeUpstreamReset(follow *UpstreamReset) *UpstreamReset {
	if follow == nil {
		return nil
	}
	credential := strings.ToLower(strings.TrimSpace(follow.Credential))
	if credential == "" {
		return nil
	}
	return &UpstreamReset{Credential: credential}
}

func cloneUpstreamReset(follow *UpstreamReset) *UpstreamReset {
	if follow == nil {
		return nil
	}
	copied := *follow
	copied.Windows = slices.Clone(follow.Windows)
	return &copied
}

func (f *UpstreamReset) validate() error {
	if f == nil {
		return nil
	}
	if !ValidCredentialFingerprint(f.Credential) {
		return invalidf("Invalid upstream auth file for reset following")
	}
	periods := make(map[int64]bool, len(f.Windows))
	for _, window := range f.Windows {
		if window.PeriodSeconds <= 0 || window.PeriodSeconds > maxPeriodSeconds || periods[window.PeriodSeconds] ||
			window.ResetAt.IsZero() || window.ObservedAt.IsZero() ||
			window.UsedPercent < 0 || math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) {
			return invalidf("Invalid upstream reset following data")
		}
		periods[window.PeriodSeconds] = true
	}
	return nil
}

// UpstreamCredentials lists the auth files that plans follow, optionally only
// the one followed by the plan bound to scope.
func (s *Store) UpstreamCredentials(scope string) []string {
	scope = normalizeScope(scope)
	var credentials []string
	s.read(func(state *State) {
		planID := ""
		if scope != "" {
			key := state.Keys[scope]
			if key == nil || key.PlanID == "" {
				return
			}
			planID = key.PlanID
		}
		for _, plan := range state.Plans {
			if plan.UpstreamReset != nil && (planID == "" || plan.ID == planID) && !slices.Contains(credentials, plan.UpstreamReset.Credential) {
				credentials = append(credentials, plan.UpstreamReset.Credential)
			}
		}
	})
	return credentials
}

// ObserveUpstream applies one observation of a followed auth file to every
// plan that follows it.
func (s *Store) ObserveUpstream(credential string, observedAt time.Time, observations []UpstreamObservation) []UpstreamFollowResult {
	credential = strings.ToLower(strings.TrimSpace(credential))
	if credential == "" {
		return nil
	}
	if observedAt.IsZero() {
		observedAt = s.Now()
	}
	observedAt = observedAt.UTC()
	type outcome struct {
		results []UpstreamFollowResult
		names   []string
		scopes  []string
	}
	current := updateResult(s, func(state *State) (outcome, Changes) {
		var current outcome
		var changes Changes
		for i := range state.Plans {
			if state.Plans[i].UpstreamReset == nil || state.Plans[i].UpstreamReset.Credential != credential {
				continue
			}
			result, scopes, persist := state.followUpstream(i, observedAt, observations)
			changes.Plans = changes.Plans || persist
			current.scopes = append(current.scopes, scopes...)
			if len(result.ResetWindows) > 0 || len(result.AlignedWindows) > 0 {
				current.results = append(current.results, result)
				current.names = append(current.names, cmp.Or(state.Plans[i].Name, state.Plans[i].ID))
			}
		}
		changes.Keys = current.scopes
		return current, changes
	})
	for _, scope := range current.scopes {
		s.blocked.clear(scope)
	}
	for i, result := range current.results {
		if len(result.ResetWindows) > 0 {
			s.AddPluginLog(PluginLogInfo, "Followed an early upstream reset for subscription plan %s: cleared %s for %d API keys",
				current.names[i], strings.Join(result.ResetWindows, ", "), result.ResetKeys)
		}
		if len(result.AlignedWindows) > 0 {
			s.AddPluginLog(PluginLogDebug, "Aligned the shared schedule of subscription plan %s with the upstream reset time: %s",
				current.names[i], strings.Join(result.AlignedWindows, ", "))
		}
	}
	return current.results
}

// followUpstream replaces the plan at index instead of mutating it: plan
// copies handed out by the store share nothing with the live state.
func (s *State) followUpstream(index int, at time.Time, observations []UpstreamObservation) (UpstreamFollowResult, []string, bool) {
	plan := clonePlan(s.Plans[index])
	follow := plan.UpstreamReset
	result := UpstreamFollowResult{PlanID: plan.ID}
	persist := false
	resets := make(map[int64]time.Time)
	started := make(map[int64]time.Time)
	for _, observation := range observations {
		if observation.PeriodSeconds <= 0 || observation.PeriodSeconds > maxPeriodSeconds || observation.ResetAt.IsZero() ||
			observation.UsedPercent < 0 || math.IsNaN(observation.UsedPercent) || math.IsInf(observation.UsedPercent, 0) {
			continue
		}
		index := slices.IndexFunc(follow.Windows, func(window UpstreamWindow) bool { return window.PeriodSeconds == observation.PeriodSeconds })
		var previous *UpstreamWindow
		if index >= 0 {
			previous = &follow.Windows[index]
			// Observations of one credential are serialized; ignore a clock that went back.
			if !at.After(previous.ObservedAt) {
				continue
			}
		}
		next, resetAt, reset := classifyUpstreamWindow(previous, observation, at)
		switch {
		case next == nil && index >= 0:
			follow.Windows = slices.Delete(follow.Windows, index, index+1)
			persist = true
		case next != nil && index < 0:
			follow.Windows = append(follow.Windows, *next)
			persist = true
		case next != nil:
			persist = persist || !next.ResetAt.Equal(previous.ResetAt)
			follow.Windows[index] = *next
		}
		if reset {
			resets[observation.PeriodSeconds] = resetAt
		}
		if next != nil {
			started[observation.PeriodSeconds] = next.ResetAt
		}
	}
	slices.SortFunc(follow.Windows, func(a, b UpstreamWindow) int { return cmp.Compare(a.PeriodSeconds, b.PeriodSeconds) })

	var scopes []string
	resetScopes := make(map[string]bool)
	for w := range plan.Windows {
		window := &plan.Windows[w]
		resetAt, reset := resets[window.PeriodSeconds]
		endAt, running := started[window.PeriodSeconds]
		aligned := running && !window.CycleAnchorAt.IsZero() && !anchorMatches(window.CycleAnchorAt, endAt, window.PeriodSeconds)
		if !reset && !aligned {
			continue
		}
		if aligned {
			window.CycleAnchorAt = time.Unix(endAt.Unix(), 0).UTC()
			result.AlignedWindows = append(result.AlignedWindows, window.Name)
			persist = true
		}
		for scope, key := range s.Keys {
			if key == nil || key.PlanID != plan.ID {
				continue
			}
			cycle, exists := key.Cycles[window.ID]
			if !exists {
				continue
			}
			switch {
			case reset && cycle.StartAt.Before(resetAt) && cycle.UsageSince.Before(resetAt):
				// The cycle began before the upstream reset.
				delete(key.Cycles, window.ID)
				resetScopes[scope] = true
			case aligned && !at.Before(cycle.EndAt):
				delete(key.Cycles, window.ID)
			case aligned:
				// Keep the usage and move the cycle onto the upstream schedule.
				shifted := window.newCycle(plan.ID, at)
				shifted.UsageSince = cycle.UsageSince
				if shifted.UsageSince.Before(shifted.StartAt) {
					shifted.UsageSince = shifted.StartAt
				}
				shifted.SpentUSD, shifted.UsedTokens, shifted.UsedRequests = cycle.SpentUSD, cycle.UsedTokens, cycle.UsedRequests
				key.Cycles[window.ID] = shifted
			default:
				continue
			}
			scopes = append(scopes, scope)
		}
		if reset {
			result.ResetWindows = append(result.ResetWindows, window.Name)
			follow.LastResetAt = at
			persist = true
		}
	}
	result.ResetKeys = len(resetScopes)
	s.Plans[index] = plan
	return result, dedupe(scopes), persist
}

// classifyUpstreamWindow compares an observation with the previous started
// window. It returns the next baseline and, for an early reset, when the reset
// took effect. An idle window has no usage and a full period ahead: Codex
// starts it at the next request, so it has no baseline.
func classifyUpstreamWindow(previous *UpstreamWindow, observation UpstreamObservation, at time.Time) (*UpstreamWindow, time.Time, bool) {
	period := time.Duration(observation.PeriodSeconds) * time.Second
	idle := observation.UsedPercent == 0 && !observation.ResetAt.Before(at.Add(period-upstreamResetTolerance))
	var next *UpstreamWindow
	if !idle {
		next = &UpstreamWindow{PeriodSeconds: observation.PeriodSeconds, ResetAt: observation.ResetAt.UTC(), UsedPercent: observation.UsedPercent, ObservedAt: at}
	}
	if previous == nil {
		return next, time.Time{}, false
	}
	end := previous.ResetAt
	switch {
	case idle:
		// Usage cleared before the window ended; afterwards it simply expired.
		return nil, at, at.Before(end.Add(-upstreamResetTolerance))
	case absDuration(observation.ResetAt.Sub(end)) <= upstreamResetTolerance:
		next.ResetAt = end
		dropped := previous.UsedPercent-observation.UsedPercent >= upstreamUsageDropPoints &&
			observation.UsedPercent <= previous.UsedPercent/10
		return next, at, dropped
	case observation.ResetAt.After(end) && observation.ResetAt.Add(-period).Before(end.Add(-upstreamResetTolerance)):
		// A new window began before the previous one ended. The reset took effect
		// by the new window's start, unless that contradicts the previous
		// observation; then only the time of this check is certain.
		resetAt := observation.ResetAt.Add(-period)
		if !resetAt.After(previous.ObservedAt) || resetAt.After(at) {
			resetAt = at
		}
		return next, resetAt, true
	default:
		// A window that began after the previous end is a scheduled rollover.
		return next, time.Time{}, false
	}
}

func anchorMatches(anchor, endAt time.Time, periodSeconds int64) bool {
	offset := (endAt.Unix() - anchor.Unix()) % periodSeconds
	if offset < 0 {
		offset += periodSeconds
	}
	tolerance := int64(upstreamResetTolerance / time.Second)
	return offset <= tolerance || periodSeconds-offset <= tolerance
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
