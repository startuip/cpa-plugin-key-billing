package plugin

import (
	"time"

	"cpa-key-billing/internal/billing"
)

const resetSyncInterval = 30 * time.Minute

// Injectable timers let tests advance thirty minutes without wall-clock sleeps.
type resetSyncTimer interface {
	C() <-chan time.Time
	Reset(time.Duration)
	Stop()
}

type resetTimer struct{ timer *time.Timer }

func newResetTimer(interval time.Duration) resetSyncTimer {
	return &resetTimer{timer: time.NewTimer(interval)}
}

func (t *resetTimer) C() <-chan time.Time   { return t.timer.C }
func (t *resetTimer) Reset(d time.Duration) { t.timer.Reset(d) }
func (t *resetTimer) Stop()                 { t.timer.Stop() }

type resetSyncWorker struct {
	stop chan struct{}
	done chan struct{}
}

func resetSyncStopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// Called under callsMu, so storage and hostCaller cannot change underneath us.
func (a *App) startResetSync(immediate bool) {
	a.resetSyncMu.Lock()
	defer a.resetSyncMu.Unlock()
	if a.resetSyncBlocked || a.resetSync != nil || a.hostCaller == nil ||
		!a.store.Enabled() || a.store.ResetFollowSyncPaused() || len(a.store.FollowedAccounts()) == 0 {
		return
	}
	worker := &resetSyncWorker{stop: make(chan struct{}), done: make(chan struct{})}
	a.resetSync = worker
	go a.runResetSync(worker, immediate)
}

// lifecycleMu serializes stop/resume. Blocking new starts before draining also
// covers a configuration save that is still running in a foreground call.
func (a *App) stopResetSync() {
	a.resetSyncMu.Lock()
	a.resetSyncBlocked = true
	worker := a.resetSync
	if worker != nil {
		close(worker.stop)
	}
	a.resetSyncMu.Unlock()
	if worker != nil {
		// Host HTTP calls have no cancellation/timeout argument. Join the
		// issued call rather than abandoning a goroutine that can touch storage.
		<-worker.done
		a.resetSyncMu.Lock()
		a.resetSync = nil
		a.resetSyncMu.Unlock()
	}
}

func (a *App) resumeResetSync() {
	a.resetSyncMu.Lock()
	a.resetSyncBlocked = false
	a.resetSyncMu.Unlock()
	// Persisted followers are synchronized once when the plugin resumes.
	a.startResetSync(true)
}

func (a *App) runResetSync(worker *resetSyncWorker, immediate bool) {
	defer close(worker.done)
	if immediate {
		a.resetSyncRound(worker.stop)
	}
	if resetSyncStopped(worker.stop) {
		return
	}
	timer := a.newResetSyncTimer(resetSyncInterval)
	defer timer.Stop()
	for {
		select {
		case <-worker.stop:
			return
		case <-timer.C():
			a.resetSyncRound(worker.stop)
			if resetSyncStopped(worker.stop) {
				return
			}
			// Wait a full interval after completion. Slow rounds never overlap
			// and missed intervals never become a burst of catch-up queries.
			timer.Reset(resetSyncInterval)
		}
	}
}

func (a *App) resetSyncRound(stop <-chan struct{}) {
	a.callsMu.RLock()
	defer a.callsMu.RUnlock()
	defer func() {
		if recover() != nil {
			// Do not log an arbitrary panic value: host callbacks may carry secrets.
			a.store.AddPluginLog(billing.PluginLogError, "Automatic upstream reset synchronization panicked; retrying next interval")
		}
	}()
	if resetSyncStopped(stop) || a.quiesced || !a.store.Enabled() || a.store.ResetFollowSyncPaused() {
		return
	}
	// Callback IDs belong to foreground host requests and expire with them.
	a.syncResetFollowers(ManagementRequest{}, viewAccess{}, stop)
}
