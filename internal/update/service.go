package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Interval is the shortest gap between two checks. Once a day, which is what
// m55.md asks for and what the enforcing UPDATE in ClaimUpdateCheck is given.
//
// The ticker that drives the pass is faster than this on purpose (see
// cmd/linkctrl/jobs.go): a job whose ticker *is* its period drifts a day later
// every restart, and on an instance that is redeployed most days it would never
// check at all. The bound is the database row; the ticker only decides how soon
// after the day is up the check happens.
const Interval = 24 * time.Hour

// Announcer is internal/notify's writing half, as this package needs it.
//
// The consumer owns the interface, the way internal/notify owns Enqueuer: this
// package should be able to be a client, a comparison and a daily bound without
// also knowing what an instance principal is, and a test satisfies this with a
// slice.
type Announcer interface {
	AnnounceRelease(ctx context.Context, running, available string) error
}

// Service is the daily check: claim the day, ask, compare, tell somebody.
type Service struct {
	q        *dbgen.Queries
	client   *Client
	announce Announcer
	log      *slog.Logger
	// version is what this binary reports. Held rather than read from
	// internal/build on each pass so a test can run the whole thing without a
	// linker flag.
	version string
	// interval is Interval outside tests.
	interval time.Duration
}

// Config is what a Service needs.
type Config struct {
	// Version is build.Get().Version. A build reporting "dev" never notifies,
	// which ParseVersion enforces rather than a special case here.
	Version string
	// Endpoint overrides the constant. Empty is production; only a test passes
	// anything, and there is no configuration path that reaches it.
	Endpoint string
	// Announce receives a newer release. Nil means the check still runs and
	// still records that it ran, and tells nobody — which is a shape only a test
	// builds, and it is guarded rather than left to panic.
	Announce Announcer
	Log      *slog.Logger
	// Interval overrides Interval. Zero is Interval.
	Interval time.Duration
}

func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = Interval
	}
	return &Service{
		q:        dbgen.New(pool),
		client:   NewClient(cfg.Version, cfg.Endpoint, nil),
		announce: cfg.Announce,
		log:      log,
		version:  cfg.Version,
		interval: interval,
	}
}

// Run performs at most one check.
//
// # It is never fatal and never reaches a user
//
// Every failure below returns nil after a Debug line. Nothing here blocks
// startup or shutdown — the pass runs on the scheduler, which the process does
// not wait for — and no surface in the product renders any of it. A GitHub that
// is down, rate-limited, or answering nonsense is a day on which this instance
// learns nothing, which is exactly what m55.md promises and what
// docs/deployment.md tells an air-gapped operator to expect.
//
// Debug rather than Warn is a deliberate choice about whose problem this is. A
// failed webhook is somebody's event not arriving; a failed update check is a
// question that went unanswered, and logging it loudly would train operators to
// ignore the log on instances that were never going to be able to reach GitHub.
//
// # And it never retries
//
// ClaimUpdateCheck writes the timestamp *before* the request, so a failure
// consumes the day the same way a success does. That is the whole of "no retry
// storm": there is no attempt counter to get wrong, because a second attempt
// cannot be reached until the row says a day has passed.
func (s *Service) Run(ctx context.Context) error {
	now := time.Now().UTC()

	// The claim is the enabled check as well as the daily bound. Both live in the
	// one statement so there is no window between reading "the operator allows
	// this" and acting on it — and *allows* is `IS TRUE`, so an instance where
	// nobody has answered yet (D164) is declined here exactly as a declining one
	// is. This package never learns which of the three it was, deliberately: all
	// three mean do nothing, and a caller that could tell them apart is a caller
	// that could eventually treat one of them differently.
	claimed, err := s.q.ClaimUpdateCheck(ctx, dbgen.ClaimUpdateCheckParams{
		At:       now,
		NotSince: now.Add(-s.interval),
	})
	if err != nil {
		return fmt.Errorf("update: claim the day's check: %w", err)
	}
	if claimed == 0 {
		return nil
	}

	rel, err := s.client.Latest(ctx)
	switch {
	case errors.Is(err, ErrNoRelease):
		s.log.Debug("update check: no published release to compare against")
		return nil
	case err != nil:
		s.log.Debug("update check did not complete", slog.Any("error", err))
		return nil
	}

	newer, ok := IsNewer(s.version, rel.TagName)
	if !ok {
		// Three cases, one answer: this build is up to date, this build is `dev`
		// and has no version to compare, or the tag does not parse. None of them
		// is an error surface — m55.md says an unparseable remote version is a
		// no-op — and the log line names both sides so the third is diagnosable.
		s.log.Debug("update check: nothing newer",
			slog.String("running", s.version), slog.String("latest", rel.TagName))
		return nil
	}

	if s.announce == nil {
		return nil
	}
	if err := s.announce.AnnounceRelease(ctx, s.version, newer.String()); err != nil {
		return fmt.Errorf("update: announce %s: %w", newer, err)
	}
	s.log.Info("a newer LinkCtrl has been published",
		slog.String("running", s.version), slog.String("available", newer.String()))
	return nil
}
