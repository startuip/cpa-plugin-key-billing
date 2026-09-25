package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
)

func TestV18ResetFollowMigrationPreservesHistoryAndRollsBack(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rollback"}[conflict], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v17.db")
			database, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = database.db.Exec(`
    ALTER TABLE api_keys DROP COLUMN reset_follow_json;
    DROP TABLE reset_snapshots;
    DROP TABLE upstream_resets;
    PRAGMA user_version = 17;
    INSERT INTO plans(position,id,name,windows_json) VALUES(0,'p','Plan','[{"id":"w","name":"Window","period_seconds":18000,"request_limit":3}]');
    INSERT INTO api_keys(scope,preview,plan_id,cycles_json) VALUES('dummy-scope','sk-dum…0001','p','{"w":{"plan_id":"p","start_at":"2026-09-20T12:00:00Z","end_at":"2026-09-20T17:00:00Z","used_requests":2}}');
    INSERT INTO request_events(id,at,scope,failed) VALUES(1,1,'dummy-scope',1),(2,2,'dummy-scope',0);
    INSERT INTO request_errors(request_event_id,status_code,body) VALUES(1,502,'dummy failure');`)
			if err != nil {
				t.Fatal(err)
			}
			if conflict {
				if _, err := database.db.Exec(`CREATE TABLE reset_snapshots (historical_data TEXT); INSERT INTO reset_snapshots VALUES('retain me')`); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if conflict {
				if err == nil {
					reopened.Close()
					t.Fatal("incompatible schema accepted")
				}
				raw, err := sql.Open("sqlite3", path)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				var version, columns, events int
				var historical string
				if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 17 {
					t.Fatalf("version = %d: %v", version, err)
				}
				if err := raw.QueryRow(`SELECT count(*) FROM pragma_table_info('api_keys') WHERE name='reset_follow_json'`).Scan(&columns); err != nil || columns != 0 {
					t.Fatal("partial ALTER TABLE escaped rollback")
				}
				if err := raw.QueryRow(`SELECT historical_data FROM reset_snapshots`).Scan(&historical); err != nil || historical != "retain me" {
					t.Fatal("conflicting table lost")
				}
				if err := raw.QueryRow(`SELECT count(*) FROM request_events`).Scan(&events); err != nil || events != 2 {
					t.Fatal("history lost during rollback")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			state := mustLoad(t, reopened)
			if state.RequestEventCount != 2 || state.State.Keys["dummy-scope"].Cycles["w"].UsedRequests != 2 || state.State.Keys["dummy-scope"].ResetFollow != nil {
				t.Fatalf("migration changed history: %+v", state)
			}
			var failures int
			if err := reopened.db.QueryRow(`SELECT count(*) FROM request_errors WHERE body='dummy failure'`).Scan(&failures); err != nil || failures != 1 {
				t.Fatal("failure detail lost")
			}
		})
	}
}

func TestResetFollowPersistenceAndWaitingCycles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	state := billing.NewState()
	state.Plans = []billing.Plan{{ID: "p", Name: "Plan", Windows: []billing.QuotaWindow{{ID: "w", Name: "5 hours", PeriodSeconds: 18000, RequestLimit: 3}}}}
	failure := billing.ResetError(messages.Literal("dummy unavailable diagnostic"))
	follow := &billing.ResetFollow{AuthIndex: "dummy-index", Provider: "codex", CredentialRef: billing.CredentialFingerprint("dummy-id"), SyncedAt: now, Error: failure,
		Windows: map[string]billing.FollowWindow{"w": {UpstreamID: "primary", PeriodSeconds: 18000, LastResetAt: now}}}
	state.Keys["dummy-scope"] = &billing.KeyState{Preview: "sk-dum…0001", PlanID: "p", ResetFollow: follow,
		Cycles: map[string]billing.QuotaCycle{"w": {PlanID: "p", StartAt: now, UsageSince: now, UsedRequests: 2, UsedTokens: 500, SpentUSD: 0.01}}}
	state.ResetSnapshots["dummy-index"] = billing.ResetSnapshot{AuthIndex: "dummy-index", Provider: "codex", CredentialRef: follow.CredentialRef, AttemptedAt: now, SyncedAt: now, Error: failure}
	state.UpstreamResets["dummy-index:dummy-operation"] = billing.UpstreamReset{AuthIndex: "dummy-index", ID: "dummy-operation", StartedAt: now, CompletedAt: now, Applied: true}
	if err := database.Save(state, billing.Changes{Plans: true, AllKeys: true, ResetSnapshots: []string{"dummy-index"}, UpstreamResets: []string{"dummy-index:dummy-operation"}}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	loaded := mustLoad(t, database).State
	key := loaded.Keys["dummy-scope"]
	if key.ResetFollow.CredentialRef != follow.CredentialRef || key.ResetFollow.Error.Text != failure.Text || key.Cycles["w"].UsedRequests != 2 || !key.Cycles["w"].EndAt.IsZero() {
		t.Fatalf("lost reset state: %+v", key)
	}
	if loaded.ResetSnapshots["dummy-index"].Error.Text != failure.Text || !loaded.UpstreamResets["dummy-index:dummy-operation"].Applied {
		t.Fatal("lost synchronization or deduplication state")
	}
}

func TestResetSnapshotsPersistOnlyForFollowedAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	now := time.Now().UTC()
	open := func() *billing.Store {
		store := billing.NewStore(func(path string) (billing.Repository, error) { return Open(path) }, func(context.Context) ([]byte, error) {
			return nil, errors.New("offline")
		})
		cfg := billing.DefaultConfig()
		cfg.StateFile = path
		if err := store.Configure(cfg); err != nil {
			t.Fatal(err)
		}
		return store
	}
	rows := func() []string {
		database, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		result, err := database.Query("SELECT auth_index FROM reset_snapshots ORDER BY auth_index")
		if err != nil {
			t.Fatal(err)
		}
		defer result.Close()
		var indexes []string
		for result.Next() {
			var index string
			if err := result.Scan(&index); err != nil {
				t.Fatal(err)
			}
			indexes = append(indexes, index)
		}
		return indexes
	}
	snapshot := func(index string) billing.ResetSnapshot {
		return billing.ResetSnapshot{AuthIndex: index, Provider: "codex", CredentialRef: billing.CredentialFingerprint(index), AttemptedAt: now, SyncedAt: now,
			Windows: []billing.UpstreamWindow{{ID: "primary_window", PeriodSeconds: 18000, ResetAt: now.Add(time.Hour)}}}
	}
	store := open()
	if _, err := store.SyncKeys([]string{"sk-dummy-snapshot-key-000001"}, false); err != nil {
		t.Fatal(err)
	}
	scope := billing.CallerScope("sk-dummy-snapshot-key-000001")
	if _, err := store.CreatePlanWithBindings(billing.Plan{Name: "Follow", Windows: []billing.QuotaWindow{{Name: "5 hours", PeriodSeconds: 18000, RequestLimit: 3}}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	store.ApplyResetSnapshot(snapshot("dummy-queried"))
	store.ApplyResetSnapshot(snapshot("dummy-followed"))
	if got := rows(); len(got) != 0 {
		t.Fatalf("unfollowed snapshots were saved: %v", got)
	}
	if err := store.SetResetFollow(scope, "dummy-followed"); err != nil {
		t.Fatal(err)
	}
	if got := rows(); len(got) != 1 || got[0] != "dummy-followed" {
		t.Fatalf("followed snapshot rows = %v", got)
	}
	store.Close()
	// Earlier versions saved a snapshot for every queried account.
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO reset_snapshots VALUES ('dummy-legacy', '{"auth_index":"dummy-legacy","provider":"kimi"}')`); err != nil {
		t.Fatal(err)
	}
	database.Close()
	store = open()
	if got := rows(); len(got) != 1 || got[0] != "dummy-followed" {
		t.Fatalf("loading kept unfollowed rows: %v", got)
	}
	if err := store.SetResetFollow(scope, ""); err != nil {
		t.Fatal(err)
	}
	if got := rows(); len(got) != 0 {
		t.Fatalf("snapshot kept after following ended: %v", got)
	}
	store.Close()
}
