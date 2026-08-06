// Package pgerr classifies Postgres errors, and it is one function.
//
// It exists as its own leaf rather than in internal/store because store's
// package proper carries goose, pgx/stdlib, and an embedded migrations
// filesystem; every service that wanted to ask "was that a duplicate key?"
// would link a migration runner to do it. It is not in the sqlc output
// directory because that directory is generated and regeneration owns it.
// A leaf with a single stdlib import is the whole point: any service package
// can depend on it without acquiring anything else.
//
// The match is on the SQLState interface, not on *pgconn.PgError. Today
// pgconn's error is the only type in the dependency tree that implements
// SQLState() string, so the two spellings accept exactly the same errors —
// but the interface form also survives a driver swap or a store layer that
// starts wrapping database errors in a type of its own, and it keeps pgconn
// out of this package's imports entirely. Before this package existed, six
// services carried private copies of this predicate, five on the interface
// form and one on the concrete type; a fix applied to one copy had five
// chances to miss.
package pgerr

import "errors"

// IsUniqueViolation reports whether err is, or wraps, a database error with
// SQLState 23505 (unique_violation). Callers use it to turn a constraint
// hit into an "already exists" answer instead of a 500 — so it must stay
// false for every other constraint class, or a foreign-key failure would be
// reported to the user as a duplicate.
func IsUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
