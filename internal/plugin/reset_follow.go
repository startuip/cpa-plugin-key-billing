package plugin

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
)

// All paths that read quota or consume reset credits share an account lock.
// Network calls never hold the billing store lock. A query cannot straddle a
// manual reset, and its result is applied only to keys still following it.
func (a *App) lockResetAccount(index string) func() {
	value, _ := a.resetAccounts.LoadOrStore(index, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

// API key users reach an upstream account at most once per interval; within it
// they share the last result, including a failure, from any caller. Admin
// queries, synchronization and reset refreshes always query and renew it.
const accountQueryInterval = time.Minute

type recentAccountQuery struct {
	at         time.Time
	credential string
	result     authQuotaResponse
	err        error
}

// A replaced credential file is queried afresh.
func accountQueryCredential(file hostAuthFile) string {
	return file.ID + "\x00" + authFileRevision(file)
}

// Called with the account lock held, after the caller's access was checked.
func (a *App) recentAccountQuery(file hostAuthFile) (recentAccountQuery, bool) {
	a.recentQueriesMu.Lock()
	defer a.recentQueriesMu.Unlock()
	recent, ok := a.recentQueries[file.AuthIndex]
	age := a.now().Sub(recent.at)
	recent.result = cloneAuthQuota(recent.result)
	return recent, ok && recent.credential == accountQueryCredential(file) && age >= 0 && age < accountQueryInterval
}

// Responses are masked in place for API key users; never share their rows.
func cloneAuthQuota(result authQuotaResponse) authQuotaResponse {
	result.Quota = slices.Clone(result.Quota)
	result.RateLimitResetCredits = slices.Clone(result.RateLimitResetCredits)
	return result
}

func (a *App) rememberAccountQuery(file hostAuthFile, result authQuotaResponse, err error) {
	a.recentQueriesMu.Lock()
	defer a.recentQueriesMu.Unlock()
	now := a.now()
	for index, recent := range a.recentQueries {
		if now.Sub(recent.at) >= accountQueryInterval {
			delete(a.recentQueries, index)
		}
	}
	a.recentQueries[file.AuthIndex] = recentAccountQuery{at: now, credential: accountQueryCredential(file), result: cloneAuthQuota(result), err: err}
}

// API key users may start one new reset per account per interval. Retrying a
// reset that already succeeded answers without contacting the provider.
func (a *App) resetAttemptWait(authIndex string) time.Duration {
	a.recentQueriesMu.Lock()
	defer a.recentQueriesMu.Unlock()
	last, ok := a.recentResets[authIndex]
	if wait := accountQueryInterval - a.now().Sub(last); ok && wait > 0 {
		return wait
	}
	return 0
}

func (a *App) rememberResetAttempt(authIndex string) {
	a.recentQueriesMu.Lock()
	defer a.recentQueriesMu.Unlock()
	now := a.now()
	for index, last := range a.recentResets {
		if now.Sub(last) >= accountQueryInterval {
			delete(a.recentResets, index)
		}
	}
	a.recentResets[authIndex] = now
}

func (a *App) queryResetAccount(callbackID string, file hostAuthFile) (authQuotaResponse, error) {
	result, err := a.fetchAuthQuota(callbackID, file, authCategory(file.Type))
	a.rememberAccountQuery(file, result, err)
	snapshot := billing.ResetSnapshot{
		AuthIndex: file.AuthIndex, Provider: authCategory(file.Type), CredentialRef: billing.CredentialFingerprint(file.ID),
		AttemptedAt: result.FetchedAt, SyncedAt: result.FetchedAt,
	}
	if snapshot.AttemptedAt.IsZero() {
		snapshot.AttemptedAt = a.store.Now()
	}
	if err != nil {
		snapshot.Error = billing.ResetError(messages.FromError(err))
	} else {
		for _, row := range result.Quota {
			if !row.Ordinary || row.PeriodSeconds <= 0 {
				continue
			}
			resetAt, _ := time.Parse(time.RFC3339, row.ResetAt)
			snapshot.Windows = append(snapshot.Windows, billing.UpstreamWindow{ID: row.WindowID, PeriodSeconds: row.PeriodSeconds, ResetAt: resetAt})
		}
	}
	a.store.ApplyResetSnapshot(snapshot)
	return result, err
}

func (a *App) setResetFollow(req ManagementRequest) ManagementResponse {
	var body struct {
		Scope     string `json:"scope"`
		AuthIndex string `json:"auth_index"`
	}
	if err := decodeStrict(req.Body, &body); err != nil {
		return errorResponse(err)
	}
	body.AuthIndex = strings.TrimSpace(body.AuthIndex)
	key, exists := a.store.KeyViewForScope(body.Scope)
	if !exists || body.AuthIndex != "" && key.PlanID == "" {
		return JSONError(http.StatusBadRequest, "invalid", "The API key does not exist or has no subscription plan")
	}

	return a.applyResetFollow(req, body.Scope, body.AuthIndex)
}

func (a *App) applyResetFollow(req ManagementRequest, scope, authIndex string) ManagementResponse {
	if authIndex != "" {
		if len(authIndex) > 512 {
			return JSONError(http.StatusBadRequest, "invalid", "Invalid auth file identifier")
		}
		unlock := a.lockResetAccount(authIndex)
		defer unlock()
		lookup := req
		lookup.Query = make(map[string][]string)
		lookup.Query.Set("auth_index", authIndex)
		selected, failure := a.resolveQuotaAuthFile(lookup, viewAccess{})
		if selected == nil {
			return failure
		}
		if category := authCategory(selected.Type); category != "codex" && category != "claude" {
			return JSONError(http.StatusBadRequest, "invalid", "Reset following supports only Codex and Claude accounts")
		}
		decision := a.store.ResolveRouting(scope, "", "")
		if decision.ConfigurationError != "" || !routingAllowsAuthFile(*selected, decision) {
			return JSONError(http.StatusBadRequest, "invalid", "The followed account must be allowed by this API key's routing rules")
		}
		if _, err := a.queryResetAccount(req.HostCallbackID, *selected); err != nil {
			return viewDetailedError(viewAccess{}, http.StatusBadGateway, "quota_failed", err)
		}
	}
	if err := a.store.SetResetFollow(scope, authIndex); err != nil {
		return errorResponse(err)
	}
	// Saving already synchronized this account. The first background round
	// waits thirty minutes instead of querying it a second time immediately.
	a.startResetSync(false)
	return JSONResponse(http.StatusOK, map[string]bool{"updated": true})
}

// A draining lifecycle call waits for this refresh while requests queue behind
// it, so the refresh stops before the next account instead of querying them all.
func (a *App) refreshResetFollowers(req ManagementRequest, access viewAccess) {
	drain := a.drainSignal()
	a.syncResetFollowers(req, access, func() bool { return resetSyncStopped(drain) || !a.store.Enabled() })
}

// Foreground and scheduled rounds use the same account locks and snapshots.
// stopped is checked before each account: an issued query always finishes, and
// no further account starts after it.
func (a *App) syncResetFollowers(req ManagementRequest, access viewAccess, stopped func() bool) {
	if !a.store.Enabled() {
		return
	}
	var accounts []string
	if access.APIKey {
		if !access.Tracked || access.Key.ResetFollow == nil {
			return
		}
		accounts = []string{access.Key.ResetFollow.AuthIndex}
	} else {
		accounts = a.store.FollowedAccounts()
	}
	// One auth file list serves every account in this pass.
	var files []hostAuthFile
	var errList error
	listed := false
	for _, index := range accounts {
		if stopped() {
			return
		}
		func() {
			unlock := a.lockResetAccount(index)
			defer unlock()
			if stopped() {
				return
			}
			if access.APIKey {
				key, ok := a.store.KeyViewForScope(access.Scope)
				if !ok || key.ResetFollow == nil || key.ResetFollow.AuthIndex != index {
					return
				}
			}
			// Resolve by the exact host index and recheck the caller's routing.
			if !listed {
				files, errList = a.listHostAuthFiles()
				listed = true
			}
			selected, _ := a.selectQuotaAuthFile(files, errList, index, access)
			if selected == nil {
				a.store.ApplyResetSnapshot(billing.ResetSnapshot{AuthIndex: index, AttemptedAt: a.store.Now(), Error: billing.ResetError(messages.New("The followed upstream account is unavailable"))})
				return
			}
			if access.APIKey {
				if _, recent := a.recentAccountQuery(*selected); recent {
					// The follow status already reflects that query.
					return
				}
			}
			_, _ = a.queryResetAccount(req.HostCallbackID, *selected)
		}()
	}
}

func (a *App) resetFollowAuthFiles(req ManagementRequest) ManagementResponse {
	scope := strings.TrimSpace(req.Query.Get("scope"))
	if scope == "" {
		return a.authFiles(viewAccess{})
	}
	key, ok := a.store.KeyViewForScope(scope)
	if !ok {
		return JSONError(http.StatusNotFound, "not_found", "The API key does not exist or has no subscription plan")
	}
	return a.authFiles(viewAccess{APIKey: true, Tracked: true, Scope: scope, Key: key})
}
