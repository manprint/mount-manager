// Package recovery detaches a stale FUSE mount and restarts the compose
// stack to bring the managed mount back to a healthy state.
package recovery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"mount-manager/internal/config"
	"mount-manager/internal/mountinfo"
)

// maxDetach bounds the number of detach attempts CleanupMount makes before
// giving up.
const maxDetach = 5

// ComposeOps is the subset of compose.Client used by Recoverer.
type ComposeOps interface {
	Down(ctx context.Context, stop time.Duration) error
	Kill(ctx context.Context) error
	Up(ctx context.Context, wait time.Duration) error
}

// Deps are the external dependencies of a Recoverer.
type Deps struct {
	Mounts  func() ([]mountinfo.Entry, error)
	Abort   func(minor int) error
	Detach  func(path string) error
	Compose ComposeOps
	Log     *slog.Logger
}

// Recoverer performs mount cleanup and compose stack recovery.
type Recoverer struct {
	cfg *config.Config
	d   Deps
}

// New builds a Recoverer from cfg and its dependencies.
func New(cfg *config.Config, d Deps) *Recoverer {
	return &Recoverer{cfg: cfg, d: d}
}

// CleanupMount detaches the expected-fstype mounts stacked at cfg.MountPath,
// retrying up to maxDetach times. If abort is true, it aborts the FUSE
// connection before each detach attempt. A foreign mount (fstype other than
// cfg.ExpectedFSType) is never touched [I-2]: if one sits on top of an
// expected mount, CleanupMount refuses; if it only sits underneath (e.g.
// mount_path is itself a bind mount), there is nothing left to clean.
func (r *Recoverer) CleanupMount(ctx context.Context, abort bool) error {
	for i := 0; i < maxDetach; i++ {
		entries, err := r.d.Mounts()
		if err != nil {
			return fmt.Errorf("read mountinfo: %w", err)
		}
		top, ok := mountinfo.Top(entries, r.cfg.MountPath)
		if !ok || !r.hasExpected(entries) {
			return nil
		}
		if top.FSType != r.cfg.ExpectedFSType {
			return fmt.Errorf("refusing to unmount %s: fstype %s is not %s", r.cfg.MountPath, top.FSType, r.cfg.ExpectedFSType)
		}
		if abort {
			if err := r.d.Abort(top.Minor); err != nil {
				r.d.Log.Warn("fuse abort failed", "minor", top.Minor, "error", err)
			}
		}
		if err := r.d.Detach(r.cfg.MountPath); err != nil {
			return err
		}
	}

	entries, err := r.d.Mounts()
	if err != nil {
		return fmt.Errorf("read mountinfo: %w", err)
	}
	if r.hasExpected(entries) {
		return fmt.Errorf("mount %s still present after %d detach attempts", r.cfg.MountPath, maxDetach)
	}
	return nil
}

// hasExpected reports whether any mount at cfg.MountPath has the expected
// fstype.
func (r *Recoverer) hasExpected(entries []mountinfo.Entry) bool {
	for _, e := range entries {
		if e.MountPoint == r.cfg.MountPath && e.FSType == r.cfg.ExpectedFSType {
			return true
		}
	}
	return false
}

// Recover runs the full recovery sequence: pre-down cleanup, compose down
// (with a kill fallback), post-down cleanup, and compose up.
func (r *Recoverer) Recover(ctx context.Context, attempt int, abortFirst bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.d.Log.Info("recovery: pre-down cleanup", "attempt", attempt)
	if err := r.CleanupMount(ctx, abortFirst); err != nil {
		return fmt.Errorf("pre-down cleanup: %w", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.d.Log.Info("recovery: compose down", "attempt", attempt)
	if err := r.d.Compose.Down(ctx, r.cfg.Recovery.DownTimeout.Duration); err != nil {
		r.d.Log.Warn("compose down failed, falling back to kill", "attempt", attempt, "error", err)
		if err := r.d.Compose.Kill(ctx); err != nil {
			r.d.Log.Warn("compose kill failed", "attempt", attempt, "error", err)
		}
		if err := r.d.Compose.Down(ctx, 0); err != nil {
			return fmt.Errorf("compose down failed: %w", err)
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.d.Log.Info("recovery: post-down cleanup", "attempt", attempt)
	if err := r.CleanupMount(ctx, true); err != nil {
		return fmt.Errorf("post-down cleanup: %w", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.d.Log.Info("recovery: compose up", "attempt", attempt)
	if err := r.d.Compose.Up(ctx, r.cfg.Recovery.UpWaitTimeout.Duration); err != nil {
		return fmt.Errorf("compose up failed: %w", err)
	}

	return nil
}
