// Package supervisor implements the mount-manager state machine: waiting
// for the compose stack to come up, monitoring it once up, running
// recovery on sustained failure, verifying recovery success, and backing
// off between failed recovery attempts.
package supervisor

import (
	"context"
	"log/slog"
	"time"

	"mount-manager/internal/config"
	"mount-manager/internal/health"
	"mount-manager/internal/notify"
)

// State identifies which phase of the state machine the supervisor is in.
type State int

const (
	Waiting State = iota
	Monitoring
	Verifying
	// StateBackoff is named to avoid colliding with the package-level
	// Backoff function required by the supervisor contract.
	StateBackoff
)

// String renders the state as a lowercase name.
func (s State) String() string {
	switch s {
	case Waiting:
		return "waiting"
	case Monitoring:
		return "monitoring"
	case Verifying:
		return "verifying"
	case StateBackoff:
		return "backoff"
	default:
		return "unknown"
	}
}

// Health evaluates the health-check suite.
type Health interface {
	Evaluate(ctx context.Context) health.Report
}

// Recovery performs mount cleanup and compose stack recovery.
type Recovery interface {
	Recover(ctx context.Context, attempt int, abortFirst bool) error
	CleanupMount(ctx context.Context, abort bool) error
}

// Clock provides the current time.
type Clock interface {
	Now() time.Time
}

// RealClock is a Clock backed by time.Now.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// Options configures a new Supervisor.
type Options struct {
	Config   *config.Config
	Health   Health
	Recovery Recovery
	Notifier notify.Notifier
	Clock    Clock
	Log      *slog.Logger
	Host     string
	Version  string
}

// Supervisor drives the mount-manager state machine. Only one goroutine
// may call Step (or Run) at a time; there is no internal locking.
type Supervisor struct {
	cfg *config.Config
	o   Options

	state          State
	failCount      int
	attempt        int
	graceStart     time.Time
	verifyDeadline time.Time
	backoffUntil   time.Time
}

// New builds a Supervisor from o.
func New(o Options) *Supervisor {
	return &Supervisor{cfg: o.Config, o: o}
}

// State returns the supervisor's current state.
func (s *Supervisor) State() State { return s.state }

// Start emits the startup and waiting events and sets the initial state.
func (s *Supervisor) Start() {
	s.emit(notify.EventStartup, "version "+s.o.Version, 0)
	s.emit(notify.EventWaiting, "waiting", 0)
	s.state = Waiting
}

// emit builds and delivers a notify.Event, and logs it (Info, or Warn for
// failure_detected/recovery_failed).
func (s *Supervisor) emit(event, detail string, attempt int) {
	ev := notify.Event{
		Event:     event,
		Timestamp: s.o.Clock.Now(),
		Host:      s.o.Host,
		MountPath: s.cfg.MountPath,
		Detail:    detail,
		Attempt:   attempt,
	}
	s.o.Notifier.Notify(ev)
	if event == notify.EventFailureDetected || event == notify.EventRecoveryFailed {
		s.o.Log.Warn("event", "event", event, "detail", detail, "attempt", attempt)
	} else {
		s.o.Log.Info("event", "event", event, "detail", detail, "attempt", attempt)
	}
}

// Step advances the state machine by one tick and returns the duration to
// wait before the next call.
func (s *Supervisor) Step(ctx context.Context) time.Duration {
	now := s.o.Clock.Now()

	if s.state == StateBackoff {
		if now.Before(s.backoffUntil) {
			return s.backoffUntil.Sub(now)
		}
		return s.runRecovery(ctx, true, "retry after backoff")
	}

	r := s.o.Health.Evaluate(ctx)

	switch s.state {
	case Waiting:
		if r.Idle {
			s.graceStart = time.Time{}
			if r.MountStale {
				if err := s.o.Recovery.CleanupMount(ctx, true); err == nil {
					s.emit(notify.EventStaleMountCleaned, "stale mount detached", 0)
				} else {
					s.o.Log.Error("stale mount cleanup failed", "error", err)
				}
			}
			return s.cfg.CheckInterval.Duration
		}
		if r.OK {
			s.state = Monitoring
			s.failCount = 0
			s.graceStart = time.Time{}
			s.emit(notify.EventStackUp, "all checks passed", 0)
			return s.cfg.CheckInterval.Duration
		}
		if s.graceStart.IsZero() {
			s.graceStart = now
		}
		if now.Sub(s.graceStart) >= s.cfg.StartupGrace.Duration {
			s.graceStart = time.Time{}
			s.emit(notify.EventFailureDetected, r.Summary(), 0)
			return s.runRecovery(ctx, r.HasMountFailure(), r.Summary())
		}
		return s.cfg.CheckInterval.Duration

	case Monitoring:
		if r.OK {
			s.failCount = 0
			return s.cfg.CheckInterval.Duration
		}
		if r.Idle {
			s.state = Waiting
			s.failCount = 0
			s.emit(notify.EventStackStopped, "no running containers", 0)
			return s.cfg.CheckInterval.Duration
		}
		s.failCount++
		s.o.Log.Warn("check failed", "count", s.failCount, "summary", r.Summary())
		if s.failCount >= s.cfg.FailureThreshold {
			s.failCount = 0
			s.emit(notify.EventFailureDetected, r.Summary(), 0)
			return s.runRecovery(ctx, r.HasMountFailure(), r.Summary())
		}
		return s.cfg.CheckInterval.Duration

	case Verifying:
		if r.OK {
			s.emit(notify.EventRecoverySucceeded, "all checks passed", s.attempt)
			s.attempt = 0
			s.failCount = 0
			s.state = Monitoring
			return s.cfg.CheckInterval.Duration
		}
		if !now.Before(s.verifyDeadline) {
			s.emit(notify.EventRecoveryFailed, "verify timeout: "+r.Summary(), s.attempt)
			return s.enterBackoff()
		}
		return s.cfg.CheckInterval.Duration
	}

	return s.cfg.CheckInterval.Duration
}

// runRecovery runs one recovery attempt and transitions to Verifying on
// success or Backoff on failure.
func (s *Supervisor) runRecovery(ctx context.Context, abortFirst bool, reason string) time.Duration {
	s.attempt++
	s.emit(notify.EventRecoveryStarted, reason, s.attempt)
	err := s.o.Recovery.Recover(ctx, s.attempt, abortFirst)
	if err != nil {
		s.emit(notify.EventRecoveryFailed, err.Error(), s.attempt)
		return s.enterBackoff()
	}
	s.state = Verifying
	s.verifyDeadline = s.o.Clock.Now().Add(s.cfg.Recovery.VerifyTimeout.Duration)
	return s.cfg.CheckInterval.Duration
}

// enterBackoff transitions to Backoff and returns the backoff duration.
func (s *Supervisor) enterBackoff() time.Duration {
	d := Backoff(s.attempt, s.cfg.Recovery.BackoffInitial.Duration, s.cfg.Recovery.BackoffMax.Duration)
	s.state = StateBackoff
	s.backoffUntil = s.o.Clock.Now().Add(d)
	return d
}

// Run drives Step in a loop until ctx is canceled, then emits a shutdown
// event and returns nil.
func (s *Supervisor) Run(ctx context.Context) error {
	s.Start()
loop:
	for {
		d := s.Step(ctx)
		if ctx.Err() != nil {
			break
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			break loop
		case <-t.C:
		}
	}
	s.emit(notify.EventShutdown, "signal received", 0)
	return nil
}

// Backoff computes the n-th backoff duration: min(initial*2^(n-1), max),
// with n<1 treated as 1, without overflow.
func Backoff(n int, initial, max time.Duration) time.Duration {
	if n < 1 {
		n = 1
	}
	d := initial
	for i := 1; i < n; i++ {
		if d >= max/2 {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}
