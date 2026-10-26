package store

import "context"

// RecordUpstreamFailure stores one upstream failure episode for the disaster
// alert. recovered marks the episode where the upstream came back.
func (d *DB) RecordUpstreamFailure(ctx context.Context, path string, status, failures int, recovered bool) error {
	rec := 0
	if recovered {
		rec = 1
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO upstream_errors (path, status, failures, recovered, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		path, status, failures, rec, nowUnix())
	return err
}

// UpstreamErrorWindow counts recorded failures since a unix timestamp.
func (d *DB) UpstreamErrorWindow(ctx context.Context, since int64) (int64, error) {
	var n int64
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM upstream_errors WHERE created_at >= ?`, since).Scan(&n)
	return n, err
}
