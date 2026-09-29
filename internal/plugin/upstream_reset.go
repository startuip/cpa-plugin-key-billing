package plugin

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
)

// Codex has no reset notification, so followed auth files are queried from
// host calls that need current quota: request admission and quota views. The
// interval bounds both the upstream load and how late a reset is followed.
const upstreamResetCheckInterval = 5 * time.Minute

type upstreamResetChecks struct {
	mu     sync.Mutex
	states map[string]*upstreamResetCheck
}

type upstreamResetCheck struct {
	running     bool
	attemptedAt time.Time
	checkedAt   time.Time
	err         messages.Message
}

type upstreamResetStatus struct {
	CheckedAt    time.Time        `json:"checked_at,omitzero"`
	Error        string           `json:"error,omitempty"`
	ErrorMessage messages.Message `json:"error_message,omitzero"`
}

type planRow struct {
	billing.Plan
	UpstreamStatus *upstreamResetStatus `json:"upstream_status,omitempty"`
}

func (a *App) planRows() []planRow {
	plans := a.store.Plans()
	rows := make([]planRow, 0, len(plans))
	a.upstreamChecks.mu.Lock()
	defer a.upstreamChecks.mu.Unlock()
	for _, plan := range plans {
		row := planRow{Plan: plan}
		if plan.UpstreamReset != nil {
			row.UpstreamStatus = &upstreamResetStatus{}
			if check := a.upstreamChecks.states[plan.UpstreamReset.Credential]; check != nil {
				row.UpstreamStatus.CheckedAt = check.checkedAt
				row.UpstreamStatus.Error, row.UpstreamStatus.ErrorMessage = check.err.Text, check.err
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// followUpstreamResets checks each due credential and reports whether any
// check ran. Concurrent callers never wait for a check another call started.
func (a *App) followUpstreamResets(callbackID string, credentials []string, force bool) bool {
	checked := false
	for _, credential := range credentials {
		if !a.beginUpstreamResetCheck(credential, force) {
			continue
		}
		checked = true
		observedAt, observations, err := a.observeCodexUpstream(callbackID, credential)
		a.finishUpstreamResetCheck(credential, observedAt, err)
		if err == nil {
			a.store.ObserveUpstream(credential, observedAt, observations)
		}
	}
	return checked
}

func (a *App) beginUpstreamResetCheck(credential string, force bool) bool {
	now := a.store.Now()
	a.upstreamChecks.mu.Lock()
	defer a.upstreamChecks.mu.Unlock()
	if a.upstreamChecks.states == nil {
		a.upstreamChecks.states = make(map[string]*upstreamResetCheck)
	}
	check := a.upstreamChecks.states[credential]
	if check == nil {
		check = &upstreamResetCheck{}
		a.upstreamChecks.states[credential] = check
	}
	if check.running || !force && !check.attemptedAt.IsZero() && now.Sub(check.attemptedAt) < upstreamResetCheckInterval {
		return false
	}
	check.running, check.attemptedAt = true, now
	return true
}

func (a *App) finishUpstreamResetCheck(credential string, observedAt time.Time, err error) {
	failure := messages.FromError(err)
	a.upstreamChecks.mu.Lock()
	check := a.upstreamChecks.states[credential]
	previous := check.err.Text
	check.running = false
	if err == nil {
		check.checkedAt, check.err = observedAt, messages.Message{}
	} else {
		check.err = failure
	}
	a.upstreamChecks.mu.Unlock()
	// Report the onset and recovery only: a failing check repeats every interval.
	if err != nil && failure.Text != previous {
		a.store.AddPluginLog(billing.PluginLogError, "Failed to check the followed upstream auth file %s: %s", shortCredentialRef(credential), failure.Text)
	} else if err == nil && previous != "" {
		a.store.AddPluginLog(billing.PluginLogInfo, "Checking the followed upstream auth file %s has recovered", shortCredentialRef(credential))
	}
}

func (a *App) followedCodexAuthFile(credential string) (hostAuthFile, error) {
	files, err := a.listHostAuthFiles()
	if err != nil {
		return hostAuthFile{}, err
	}
	return findFollowedCodexAuthFile(files, credential)
}

func findFollowedCodexAuthFile(files []hostAuthFile, credential string) (hostAuthFile, error) {
	for _, file := range files {
		if id := strings.TrimSpace(file.ID); id == "" || billing.CredentialFingerprint(id) != credential {
			continue
		}
		switch {
		case authCategory(file.Type) != "codex" || strings.EqualFold(strings.TrimSpace(file.AccountType), "api_key"):
			return hostAuthFile{}, messages.Errorf("Only Codex auth files can be followed for quota resets")
		case file.Disabled:
			return hostAuthFile{}, messages.Errorf("The followed auth file is disabled")
		case file.RuntimeOnly:
			return hostAuthFile{}, messages.Errorf("Runtime-only auth files have no readable credentials")
		}
		return file, nil
	}
	return hostAuthFile{}, messages.Errorf("The followed auth file does not exist")
}

func (a *App) observeCodexUpstream(callbackID, credential string) (time.Time, []billing.UpstreamObservation, error) {
	file, err := a.followedCodexAuthFile(credential)
	if err != nil {
		return time.Time{}, nil, err
	}
	auth, route, err := a.readAuthCredential(callbackID, file)
	if err != nil {
		return time.Time{}, nil, err
	}
	token := credentialToken(auth)
	if token == "" {
		return time.Time{}, nil, messages.Errorf("Auth file has no usable credentials")
	}
	headers := http.Header{"User-Agent": {"codex_cli_rs/0.76.0"}}
	if accountID := credentialString(auth, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"); accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	usage, err := a.upstream(route, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", token, headers, nil)
	if err != nil {
		return time.Time{}, nil, err
	}
	observedAt := a.store.Now().UTC()
	observations := codexUpstreamObservations(objectMap(usage, "rate_limit", "rateLimit"), observedAt)
	if len(observations) == 0 {
		return time.Time{}, nil, messages.Errorf("Codex returned no rate-limit windows")
	}
	return observedAt, observations, nil
}

// Only the account-wide windows are followed, not per-model or code review limits.
func codexUpstreamObservations(info map[string]any, observedAt time.Time) []billing.UpstreamObservation {
	var observations []billing.UpstreamObservation
	for _, key := range []string{"primary_window", "secondary_window"} {
		window := objectMap(info, key, camelKey(key))
		seconds, okSeconds := intValue(window, "limit_window_seconds", "limitWindowSeconds")
		used, okUsed := floatValue(window, "used_percent", "usedPercent")
		if !okSeconds || seconds <= 0 || !okUsed {
			continue
		}
		// A relative reset time is immune to clock skew against upstream.
		var resetAt time.Time
		if after, ok := intValue(window, "reset_after_seconds", "resetAfterSeconds"); ok && after >= 0 {
			resetAt = observedAt.Add(time.Duration(after) * time.Second)
		} else if at, ok := intValue(window, "reset_at", "resetAt"); ok && at > 0 {
			resetAt = time.Unix(at, 0)
		} else {
			continue
		}
		observations = append(observations, billing.UpstreamObservation{PeriodSeconds: seconds, ResetAt: resetAt.UTC(), UsedPercent: used})
	}
	return observations
}

func (a *App) syncPlanUpstreamReset(req ManagementRequest) ManagementResponse {
	var body struct {
		ID string `json:"id"`
	}
	if errDecode := decodeStrict(req.Body, &body); errDecode != nil {
		return errorResponse(errDecode)
	}
	var credential string
	for _, plan := range a.store.Plans() {
		if plan.ID == strings.TrimSpace(body.ID) && plan.UpstreamReset != nil {
			credential = plan.UpstreamReset.Credential
		}
	}
	if credential == "" {
		return JSONError(http.StatusNotFound, "not_found", "The subscription plan does not follow an upstream auth file")
	}
	a.followUpstreamResets(req.HostCallbackID, []string{credential}, true)
	return JSONResponse(http.StatusOK, map[string]any{"plans": a.planRows()})
}

// validateUpstreamCredential accepts an unchanged credential even if its auth
// file was removed, so unrelated plan edits still save.
func (a *App) validateUpstreamCredential(follow *billing.UpstreamReset, planID string) *ManagementResponse {
	if follow == nil {
		return nil
	}
	credential := strings.ToLower(strings.TrimSpace(follow.Credential))
	if credential == "" {
		return nil
	}
	for _, plan := range a.store.Plans() {
		if plan.ID == planID && plan.UpstreamReset != nil && plan.UpstreamReset.Credential == credential {
			return nil
		}
	}
	if !billing.ValidCredentialFingerprint(credential) {
		response := JSONError(http.StatusBadRequest, "invalid", "Invalid upstream auth file for reset following")
		return &response
	}
	files, err := a.listHostAuthFiles()
	if err != nil {
		response := jsonMessageError(http.StatusBadGateway, "host_unavailable", messages.FromError(err))
		return &response
	}
	if _, err := findFollowedCodexAuthFile(files, credential); err != nil {
		response := jsonMessageError(http.StatusBadRequest, "invalid", messages.FromError(err))
		return &response
	}
	return nil
}
