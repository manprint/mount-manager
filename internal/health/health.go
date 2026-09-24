// Package health evaluates the mount-manager health-check suite (C1-C5):
// mount presence/type (mountinfo), a filesystem probe run out-of-process,
// docker compose container status, an optional HTTP check and an optional
// exec-based check run inside a container.
//
// I-3: health never touches the mount filesystem directly. It only reads
// mount state through Deps.Mounts (mountinfo) and checks liveness through
// the injected Deps.Probe function, which runs out-of-process.
package health

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"mount-manager/internal/compose"
	"mount-manager/internal/config"
	"mount-manager/internal/mountinfo"
	"mount-manager/internal/probe"
)

// ComposeAPI is the subset of *compose.Client used by health checks.
type ComposeAPI interface {
	Services(ctx context.Context) ([]string, error)
	PS(ctx context.Context) ([]compose.Container, error)
	Exec(ctx context.Context, timeout time.Duration, service string, cmd ...string) ([]byte, error)
}

// Deps are the external dependencies used by Evaluate. All fields are
// expected to be non-nil except HTTP, which is nil when checks.http.url is
// empty.
type Deps struct {
	Mounts  func() ([]mountinfo.Entry, error)
	Probe   func(ctx context.Context, path, canary string) probe.Result
	Compose ComposeAPI
	HTTP    *http.Client // nil if checks.http.url is empty
}

// Failure describes one failed check. Check is one of "mount", "probe",
// "docker", "containers", "http", "exec".
type Failure struct {
	Check, Detail string
}

// Report is the outcome of one Evaluate call.
type Report struct {
	Idle, OK, MountPresent, MountStale bool
	MountEntry                         mountinfo.Entry
	Running                            int
	Failures                           []Failure
}

// Summary renders a short, human-readable status.
func (r Report) Summary() string {
	if r.OK {
		return "ok"
	}
	if r.Idle {
		return "idle"
	}
	parts := make([]string, len(r.Failures))
	for i, f := range r.Failures {
		parts[i] = f.Check + ": " + f.Detail
	}
	return strings.Join(parts, "; ")
}

// HasMountFailure reports whether Failures contains a "mount" or "probe"
// check.
func (r Report) HasMountFailure() bool {
	for _, f := range r.Failures {
		if f.Check == "mount" || f.Check == "probe" {
			return true
		}
	}
	return false
}

// Checker evaluates the health-check suite for a single configuration. It
// caches the compose services list across calls, once known.
type Checker struct {
	cfg      *config.Config
	d        Deps
	services []string
}

// New builds a Checker from cfg and d.
func New(cfg *config.Config, d Deps) *Checker {
	return &Checker{cfg: cfg, d: d}
}

// Evaluate runs the health-check suite in the mandatory order:
//  1. C1 mount presence/type (mountinfo)
//  2. C3 compose services/ps, running count, idle, per-service container checks
//  3. idle short-circuit: if idle, probe with an empty canary to detect a
//     stale mount and return immediately (C2, C4, C5 are skipped)
//  4. C2 probe with the configured canary
//  5. C4 HTTP check, if configured
//  6. C5 exec check, if configured and C2 ran with OK
//  7. OK = no failures and not idle
func (c *Checker) Evaluate(ctx context.Context) Report {
	var r Report

	// 1. C1: mount presence and type.
	entries, err := c.d.Mounts()
	if err != nil {
		r.Failures = append(r.Failures, Failure{"mount", fmt.Sprintf("read mountinfo: %s", err)})
	} else if top, ok := mountinfo.Top(entries, c.cfg.MountPath); !ok {
		r.Failures = append(r.Failures, Failure{"mount", "not mounted"})
	} else if top.FSType != c.cfg.ExpectedFSType {
		r.Failures = append(r.Failures, Failure{"mount", fmt.Sprintf("unexpected fstype %s", top.FSType)})
	} else {
		r.MountPresent = true
		r.MountEntry = top
	}

	// 2. C3: compose services + ps, running count, idle, per-service checks.
	// Any docker error leaves Idle false, so PS is skipped when Services fails.
	var dockerErr error
	if c.services == nil {
		svcs, err := c.d.Compose.Services(ctx)
		if err != nil {
			dockerErr = err
		} else if len(svcs) > 0 {
			c.services = svcs
		}
	}

	var containers []compose.Container
	if dockerErr == nil {
		containers, dockerErr = c.d.Compose.PS(ctx)
	}
	if dockerErr != nil {
		r.Failures = append(r.Failures, Failure{"docker", dockerErr.Error()})
	} else {
		running := 0
		for _, ct := range containers {
			switch ct.State {
			case "running", "restarting", "paused":
				running++
			}
		}
		r.Running = running
		r.Idle = running == 0

		for _, svc := range c.services {
			var found *compose.Container
			for i := range containers {
				if containers[i].Service == svc {
					found = &containers[i]
					break
				}
			}
			switch {
			case found == nil:
				r.Failures = append(r.Failures, Failure{"containers", svc + ": missing"})
			case found.State != "running":
				r.Failures = append(r.Failures, Failure{"containers", svc + ": state=" + found.State})
			case found.Health != "" && found.Health != "healthy":
				r.Failures = append(r.Failures, Failure{"containers", svc + ": health=" + found.Health})
			}
		}
	}

	// 3. Idle short-circuit.
	if r.Idle {
		if r.MountPresent {
			res := c.d.Probe(ctx, c.cfg.MountPath, "")
			r.MountStale = !res.OK
		}
		r.OK = false
		return r
	}

	// 4. C2: probe with the configured canary.
	var hostEntries, probeOK bool
	if r.MountPresent {
		res := c.d.Probe(ctx, c.cfg.MountPath, c.cfg.Checks.Canary)
		probeOK = res.OK
		hostEntries = res.HasEntries
		if !res.OK {
			r.Failures = append(r.Failures, Failure{"probe", res.Error})
		}
	}

	// 5. C4: HTTP check.
	if c.cfg.Checks.HTTP.URL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Checks.HTTP.URL, nil)
		if err != nil {
			r.Failures = append(r.Failures, Failure{"http", err.Error()})
		} else {
			resp, err := c.d.HTTP.Do(req)
			if err != nil {
				r.Failures = append(r.Failures, Failure{"http", err.Error()})
			} else {
				accepted := false
				for _, s := range c.cfg.Checks.HTTP.AcceptStatus {
					if s == resp.StatusCode {
						accepted = true
						break
					}
				}
				if !accepted {
					r.Failures = append(r.Failures, Failure{"http", fmt.Sprintf("status %d", resp.StatusCode)})
				}
				resp.Body.Close()
			}
		}
	}

	// 6. C5: exec check, only if C2 ran and was OK.
	if c.cfg.Checks.Exec.Service != "" && r.MountPresent && probeOK {
		svc := c.cfg.Checks.Exec.Service
		timeout := c.cfg.Checks.Exec.Timeout.Duration
		out, err := c.d.Compose.Exec(ctx, timeout, svc, "ls", "-A", "--", c.cfg.Checks.Exec.Path)
		if err != nil {
			r.Failures = append(r.Failures, Failure{"exec", err.Error()})
		} else if hostEntries && strings.TrimSpace(string(out)) == "" {
			r.Failures = append(r.Failures, Failure{"exec", "container sees empty directory while host mount has entries"})
		} else if c.cfg.Checks.Canary != "" {
			canaryPath := path.Join(c.cfg.Checks.Exec.Path, c.cfg.Checks.Canary)
			if _, err := c.d.Compose.Exec(ctx, timeout, svc, "ls", "-d", "--", canaryPath); err != nil {
				r.Failures = append(r.Failures, Failure{"exec", "canary: " + err.Error()})
			}
		}
	}

	// 7. OK.
	r.OK = len(r.Failures) == 0 && !r.Idle
	return r
}
