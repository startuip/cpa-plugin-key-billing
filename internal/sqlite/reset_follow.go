package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"cpa-key-billing/internal/billing"
)

func saveResetFollow(tx *sql.Tx, state *billing.State, changes billing.Changes) error {
	for _, index := range changes.ResetSnapshots {
		snapshot := state.ResetSnapshots[index]
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO reset_snapshots VALUES (?, ?) ON CONFLICT(auth_index) DO UPDATE SET snapshot_json = excluded.snapshot_json`, index, string(raw)); err != nil {
			return err
		}
	}
	for _, id := range changes.UpstreamResets {
		operation := state.UpstreamResets[id]
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

func (d *DB) loadResetFollow(state *billing.State) error {
	if err := loadResetObjects(d.db, `SELECT auth_index, snapshot_json FROM reset_snapshots`, state.ResetSnapshots); err != nil {
		return err
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
