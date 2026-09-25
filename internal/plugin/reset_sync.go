package plugin

import (
	"slices"
	"strings"
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

// One worker serves the open database. Settings changes wake it rather than
// replace it, so rounds never overlap, even while an issued host call, which
// cannot be cancelled, is still running.
type resetSyncWorker struct {
	stop      chan struct{} // closed before storage is switched or closed
	wake      chan struct{}
	done      chan struct{}
	immediate bool // guarded by App.resetSyncMu
	timer     resetSyncTimer
}

func resetSyncStopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

func (a *App) resetSyncEnabled() bool {
	return a.store.Enabled() && !a.store.ResetFollowSyncPaused()
}

// Called under callsMu, so storage and hostCaller cannot change underneath us.
// A running worker re-reads the settings; immediate asks it for a round now.
func (a *App) startResetSync(immediate bool) {
	a.resetSyncMu.Lock()
	defer a.resetSyncMu.Unlock()
	if a.resetSyncBlocked || a.hostCaller == nil {
		return
	}
	if worker := a.resetSync; worker != nil {
		worker.immediate = worker.immediate || immediate
		select {
		case worker.wake <- struct{}{}:
		default:
		}
		return
	}
	if !a.resetSyncEnabled() || len(a.store.FollowedAccounts()) == 0 {
		return
	}
	worker := &resetSyncWorker{stop: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{}), immediate: immediate}
	a.resetSync = worker
	go a.runResetSync(worker)
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
	}
}

func (a *App) resumeResetSync() {
	a.resetSyncMu.Lock()
	a.resetSyncBlocked = false
	a.resetSyncOwed = true
	a.resetSyncMu.Unlock()
	// Persisted followers are synchronized once when storage opens or the plugin resumes.
	a.startResetSync(true)
}

func (a *App) resetSyncStillOwed() bool {
	a.resetSyncMu.Lock()
	defer a.resetSyncMu.Unlock()
	return a.resetSyncOwed
}

// CPA registers plugins before it attaches its auth manager, and until then
// lists auth files from disk without host indexes, so no followed account can
// be resolved. The owed round waits for a later reconfiguration instead of
// reporting every followed account unavailable; CPA sends one after loading.
func (a *App) hostAuthInventoryLoaded() bool {
	files, err := a.listHostAuthFiles()
	return err == nil && slices.ContainsFunc(files, func(file hostAuthFile) bool { return strings.TrimSpace(file.AuthIndex) != "" })
}

func (a *App) runResetSync(worker *resetSyncWorker) {
	defer close(worker.done)
	rearm := false
	for {
		immediate, ok := a.continueResetSync(worker)
		if !ok {
			return
		}
		if immediate {
			a.resetSyncRound(worker)
			rearm = true
			continue
		}
		switch {
		case worker.timer == nil:
			worker.timer = a.newResetSyncTimer(resetSyncInterval)
		case rearm:
			// Wait a full interval after completion. Slow rounds never overlap
			// and missed intervals never become a burst of catch-up queries.
			worker.timer.Reset(resetSyncInterval)
		}
		rearm = false
		select {
		case <-worker.stop:
		case <-worker.wake:
		case <-worker.timer.C():
			a.resetSyncRound(worker)
			rearm = true
		}
	}
}

// The decision to exit is made under resetSyncMu, so a concurrent start either
// wakes this worker or starts a new one after it has left. Nothing but closing
// done remains once it has.
func (a *App) continueResetSync(worker *resetSyncWorker) (immediate, ok bool) {
	a.resetSyncMu.Lock()
	defer a.resetSyncMu.Unlock()
	if resetSyncStopped(worker.stop) || !a.resetSyncEnabled() {
		if worker.timer != nil {
			worker.timer.Stop()
		}
		if a.resetSync == worker {
			a.resetSync = nil
		}
		return false, false
	}
	immediate, worker.immediate = worker.immediate, false
	return immediate, true
}

func (a *App) resetSyncRound(worker *resetSyncWorker) {
	a.callsMu.RLock()
	defer a.callsMu.RUnlock()
	defer func() {
		if recover() != nil {
			// Do not log an arbitrary panic value: host callbacks may carry secrets.
			a.store.AddPluginLog(billing.PluginLogError, "Automatic upstream reset synchronization panicked; retrying next interval")
		}
	}()
	// Pausing or disabling does not join an issued query; each account checks.
	stopped := func() bool { return resetSyncStopped(worker.stop) || !a.resetSyncEnabled() }
	if a.quiesced || stopped() {
		return
	}
	if a.resetSyncStillOwed() {
		if !a.hostAuthInventoryLoaded() {
			return
		}
		a.resetSyncMu.Lock()
		a.resetSyncOwed = false
		a.resetSyncMu.Unlock()
	}
	// Callback IDs belong to foreground host requests and expire with them.
	a.syncResetFollowers(ManagementRequest{}, viewAccess{}, stopped)
}

// beginDrain tells foreground refresh loops that a lifecycle call is waiting for
// them. Callers hold lifecycleMu.
func (a *App) beginDrain() {
	a.resetSyncMu.Lock()
	defer a.resetSyncMu.Unlock()
	close(a.drain)
}

func (a *App) endDrain() {
	a.resetSyncMu.Lock()
	defer a.resetSyncMu.Unlock()
	a.drain = make(chan struct{})
}

func (a *App) drainSignal() <-chan struct{} {
	a.resetSyncMu.Lock()
	defer a.resetSyncMu.Unlock()
	return a.drain
}
