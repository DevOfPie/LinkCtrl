package pgerr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DevOfPie/LinkCtrl/internal/store/pgerr"
)

// sqlStater is deliberately not a *pgconn.PgError. It exists to pin the
// contract that IsUniqueViolation matches on the SQLState interface, not on
// pgconn's concrete type: a driver change, or a store layer that starts
// wrapping database errors in its own type, must keep working without this
// package hearing about it.
type sqlStater struct {
	code string
}

func (e sqlStater) Error() string    { return "sqlstate " + e.code }
func (e sqlStater) SQLState() string { return e.code }

func TestIsUniqueViolation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			// The case every service hits in production: pgx returns a
			// *pgconn.PgError, callers wrap it with context on the way up.
			name: "wrapped pgconn unique violation",
			err:  fmt.Errorf("insert link: %w", &pgconn.PgError{Code: "23505"}),
			want: true,
		},
		{
			// A different constraint class must not read as a duplicate:
			// callers translate true into "already exists" responses, and a
			// foreign-key failure reported that way would tell the user the
			// wrong story.
			name: "pgconn foreign key violation",
			err:  &pgconn.PgError{Code: "23503"},
			want: false,
		},
		{
			name: "plain error",
			err:  errors.New("no sql state here"),
			want: false,
		},
		{
			name: "non-pgconn type exposing SQLState",
			err:  fmt.Errorf("store: %w", sqlStater{code: "23505"}),
			want: true,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgerr.IsUniqueViolation(tc.err); got != tc.want {
				t.Fatalf("IsUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
