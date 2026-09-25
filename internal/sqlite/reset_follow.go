package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"cpa-key-billing/internal/billing"
)

// A changed ID is written when its object exists, and deleted otherwise.
// Snapshots are kept only for accounts a key follows.
func saveResetFollow(tx *sql.Tx, state *billing.State, changes billing.Changes) error {
	for _, index := range changes.ResetSnapshots {
		snapshot, exists := state.ResetSnapshots[index]
		if !exists || !state.ResetSnapshotFollowed(index) {
			if _, err := tx.Exec(`DELETE FROM reset_snapshots WHERE auth_index = ?`, index); err != nil {
				return err
			}
			continue
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO reset_snapshots VALUES (?, ?) ON CONFLICT(auth_index) DO UPDATE SET snapshot_json = excluded.snapshot_json`, index, string(raw)); err != nil {
			return err
		}
	}
	for _, id := range changes.UpstreamResets {
		operation, exists := state.UpstreamResets[id]
		if !exists {
			if _, err := tx.Exec(`DELETE FROM upstream_resets WHERE operation_key = ?`, id); err != nil {
				return err
			}
			continue
		}
		raw, err := json.Marshal(operation)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO upstream_resets VALUES (?, ?) ON CONFLICT(operation_key) DO UPDATE SET operation_json = excluded.operation_json`, id, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// Called after keys load. Snapshots of accounts no key follows are dropped,
// including those that earlier versions saved for every queried account.
func (d *DB) loadResetFollow(state *billing.State) error {
	if err := loadResetObjects(d.db, `SELECT auth_index, snapshot_json FROM reset_snapshots`, state.ResetSnapshots); err != nil {
		return err
	}
	for index := range state.ResetSnapshots {
		if state.ResetSnapshotFollowed(index) {
			continue
		}
		if _, err := d.db.Exec(`DELETE FROM reset_snapshots WHERE auth_index = ?`, index); err != nil {
			return fmt.Errorf("Remove unfollowed reset snapshot: %w", err)
		}
		delete(state.ResetSnapshots, index)
	}
	return loadResetObjects(d.db, `SELECT operation_key, operation_json FROM upstream_resets`, state.UpstreamResets)
}

func loadResetObjects[T any](db *sql.DB, query string, target map[string]T) error {
	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		var value T
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return fmt.Errorf("Read upstream reset state: %w", err)
		}
		target[id] = value
	}
	return rows.Err()
}
