package store

import "errors"

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrNoActor is returned when a claim decision carries no admin id. A credit
// with no attributable person is exactly what the audit trail exists to prevent,
// and decided_by would otherwise be 0.
var ErrNoActor = errors.New("claim decision needs a deciding admin")

// ErrRefMismatch is returned when an idempotency ref already exists for a
// different customer or a different amount. Silently replaying it would drop the
// caller's money, so the caller has to see the conflict.
var ErrRefMismatch = errors.New("ledger ref exists for a different customer or amount")
