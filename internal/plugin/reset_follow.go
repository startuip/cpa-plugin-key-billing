package plugin

import (
	"net/http"
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

func (a *App) queryResetAccount(callbackID string, file hostAuthFile) (authQuotaResponse, error) {
	result, err := a.fetchAuthQuota(callbackID, file, authCategory(file.Type))
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
	return JSONResponse(http.StatusOK, map[string]bool{"updated": true})
}

// Synchronization runs only inside an explicit host management call. In
// particular, registration/reconfiguration and model requests never start work.
// v7.2.143 does not notify plugins when disabled, so no background task is safe.
func (a *App) refreshResetFollowers(req ManagementRequest, access viewAccess) {
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
	for _, index := range accounts {
		func() {
			unlock := a.lockResetAccount(index)
			defer unlock()
			if access.APIKey {
				key, ok := a.store.KeyViewForScope(access.Scope)
				if !ok || key.ResetFollow == nil || key.ResetFollow.AuthIndex != index {
					return
				}
			}
			// Resolve by the exact host index and recheck the caller's routing.
			lookup := ManagementRequest{Query: map[string][]string{"auth_index": {index}}, HostCallbackID: req.HostCallbackID}
			selected, _ := a.resolveQuotaAuthFile(lookup, access)
			if selected == nil {
				a.store.ApplyResetSnapshot(billing.ResetSnapshot{AuthIndex: index, AttemptedAt: a.store.Now(), Error: billing.ResetError(messages.New("The followed upstream account is unavailable"))})
				return
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
