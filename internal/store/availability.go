package store

import (
	"context"
	"errors"
	"time"
)

// OfflineModel is a Model currently excluded from customer requests.
type OfflineModel struct {
	Model         string
	OfflineAt     time.Time
	NextProbeAt   time.Time
	ProbeFailures int
}

// TakeModelOffline records the first outage and makes a health check due now.
// Repeated failures do not reset the schedule of a Model already offline.
func (d *DB) TakeModelOffline(ctx context.Context, model string, now time.Time) error {
	if model == "" {
		return errors.New("model is required")
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO model_availability (model, offline_at, next_probe_at, probe_failures)
		 VALUES (?, ?, ?, 0)
		 ON CONFLICT(model) DO NOTHING`, model, now.Unix(), now.Unix())
	return err
}

// RecordModelProbeFailure keeps a Model offline and schedules its next check.
func (d *DB) RecordModelProbeFailure(ctx context.Context, model string, next time.Time) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE model_availability
		 SET probe_failures = probe_failures + 1, next_probe_at = ?
		 WHERE model = ?`, next.Unix(), model)
	return err
}

// RestoreModel removes the offline state after a successful health check.
func (d *DB) RestoreModel(ctx context.Context, model string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM model_availability WHERE model = ?`, model)
	return err
}

// IsModelOffline reports whether requests for a Model should be refused.
func (d *DB) IsModelOffline(ctx context.Context, model string) (bool, error) {
	var offline int
	err := d.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM model_availability WHERE model = ?)`, model).Scan(&offline)
	return offline != 0, err
}

// OfflineModels lists every Model whose outage must survive process restarts.
func (d *DB) OfflineModels(ctx context.Context) ([]OfflineModel, error) {
	return d.listOfflineModels(ctx,
		`SELECT model, offline_at, next_probe_at, probe_failures
		 FROM model_availability ORDER BY offline_at, model`)
}

// DueModelProbes lists offline Models whose next health check is due.
func (d *DB) DueModelProbes(ctx context.Context, now time.Time) ([]OfflineModel, error) {
	return d.listOfflineModels(ctx,
		`SELECT model, offline_at, next_probe_at, probe_failures
		 FROM model_availability WHERE next_probe_at <= ? ORDER BY next_probe_at, model`, now.Unix())
}

func (d *DB) listOfflineModels(ctx context.Context, query string, args ...any) ([]OfflineModel, error) {
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var models []OfflineModel
	for rows.Next() {
		var model OfflineModel
		var offlineAt, nextProbeAt int64
		if err := rows.Scan(&model.Model, &offlineAt, &nextProbeAt, &model.ProbeFailures); err != nil {
			return nil, err
		}
		model.OfflineAt = time.Unix(offlineAt, 0)
		model.NextProbeAt = time.Unix(nextProbeAt, 0)
		models = append(models, model)
	}
	return models, rows.Err()
}
