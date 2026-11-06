package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mount-manager/internal/compose"
	"mount-manager/internal/config"
	"mount-manager/internal/mountinfo"
	"mount-manager/internal/probe"
)

func baseConfig() *config.Config {
	return &config.Config{
		MountPath:      "/mnt/m",
		ExpectedFSType: "fuse.rclone",
		Checks: config.Checks{
			HTTP: config.HTTPCheck{
				AcceptStatus: []int{200, 401, 403},
				Timeout:      config.Duration{Duration: 5 * time.Second},
			},
			Exec: config.ExecCheck{
				Timeout: config.Duration{Duration: 10 * time.Second},
			},
		},
	}
}

func mountedEntries() []mountinfo.Entry {
	return []mountinfo.Entry{
		{MountPoint: "/mnt/m", FSType: "fuse.rclone"},
	}
}

// fakeCompose is a stub ComposeAPI implementation for tests.
type fakeCompose struct {
	servicesFn func(ctx context.Context) ([]string, error)
	psFn       func(ctx context.Context) ([]compose.Container, error)
	execFn     func(ctx context.Context, timeout time.Duration, container string, cmd ...string) ([]byte, error)

	servicesCalls int
	psCalls       int
	execCalls     int
}

func (f *fakeCompose) Services(ctx context.Context) ([]string, error) {
	f.servicesCalls++
	return f.servicesFn(ctx)
}

func (f *fakeCompose) PS(ctx context.Context) ([]compose.Container, error) {
	f.psCalls++
	return f.psFn(ctx)
}

func (f *fakeCompose) Exec(ctx context.Context, timeout time.Duration, container string, cmd ...string) ([]byte, error) {
	f.execCalls++
	if f.execFn != nil {
		return f.execFn(ctx, timeout, container, cmd...)
	}
	return nil, nil
}

func defaultServicesFn(ctx context.Context) ([]string, error) {
	return []string{"rclone-mount", "superdufs"}, nil
}

func runningContainersFn(ctx context.Context) ([]compose.Container, error) {
	return []compose.Container{
		{Name: "m-rclone-mount-1", Service: "rclone-mount", State: "running", Health: ""},
		{Name: "m-superdufs-1", Service: "superdufs", State: "running", Health: "healthy"},
	}, nil
}

func emptyPSFn(ctx context.Context) ([]compose.Container, error) {
	return nil, nil
}

// probeSpy records every Probe call and returns a fixed result.
type probeSpy struct {
	calls  []struct{ Path, Canary string }
	result probe.Result
}

func (p *probeSpy) fn(ctx context.Context, path, canary string) probe.Result {
	p.calls = append(p.calls, struct{ Path, Canary string }{path, canary})
	return p.result
}

func mountsOK() ([]mountinfo.Entry, error) { return mountedEntries(), nil }
func mountsMissing() ([]mountinfo.Entry, error) {
	return []mountinfo.Entry{}, nil
}

func TestEvaluateAllOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := baseConfig()
	cfg.Checks.Canary = "canary.txt"
	cfg.Checks.HTTP.URL = srv.URL
	cfg.Checks.Exec.Service = "rclone-mount"
	cfg.Checks.Exec.Path = "/data"

	fc := &fakeCompose{
		servicesFn: defaultServicesFn,
		psFn:       runningContainersFn,
		execFn: func(ctx context.Context, timeout time.Duration, service string, cmd ...string) ([]byte, error) {
			if len(cmd) > 0 && cmd[len(cmd)-1] == "/data/canary.txt" {
				return []byte(""), nil
			}
			return []byte("somefile\n"), nil
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true, HasEntries: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
		HTTP:    &http.Client{},
	})
	r := c.Evaluate(context.Background())

	if !r.OK {
		t.Fatalf("OK = false, Failures = %+v", r.Failures)
	}
	if r.Summary() != "ok" {
		t.Fatalf("Summary() = %q, want ok", r.Summary())
	}
	if len(sp.calls) != 1 || sp.calls[0].Canary != "canary.txt" {
		t.Fatalf("probe calls = %+v, want 1 call with canary.txt", sp.calls)
	}
}

func TestEvaluateIdleNoContainers(t *testing.T) {
	cfg := baseConfig()
	fc := &fakeCompose{servicesFn: defaultServicesFn, psFn: emptyPSFn}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsMissing,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	if !r.Idle {
		t.Fatalf("Idle = false, want true")
	}
	if r.OK {
		t.Fatalf("OK = true, want false")
	}
	if len(sp.calls) != 0 {
		t.Fatalf("probe calls = %d, want 0", len(sp.calls))
	}
}

func TestEvaluateIdleStaleMount(t *testing.T) {
	cfg := baseConfig()
	fc := &fakeCompose{servicesFn: defaultServicesFn, psFn: emptyPSFn}
	sp := &probeSpy{result: probe.Result{OK: false, Error: "stale"}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	if !r.Idle {
		t.Fatalf("Idle = false, want true")
	}
	if !r.MountStale {
		t.Fatalf("MountStale = false, want true")
	}
	if r.OK {
		t.Fatalf("OK = true, want false")
	}
	if len(sp.calls) != 1 || sp.calls[0].Canary != "" {
		t.Fatalf("probe calls = %+v, want 1 call with empty canary", sp.calls)
	}
}

func TestEvaluateMountMissingWhileRunning(t *testing.T) {
	cfg := baseConfig()
	cfg.Checks.Exec.Service = "rclone-mount"
	cfg.Checks.Exec.Path = "/data"
	fc := &fakeCompose{servicesFn: defaultServicesFn, psFn: runningContainersFn}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsMissing,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	found := false
	for _, f := range r.Failures {
		if f.Check == "mount" && f.Detail == "not mounted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want mount: not mounted", r.Failures)
	}
	if len(sp.calls) != 0 {
		t.Fatalf("probe calls = %d, want 0", len(sp.calls))
	}
	if fc.execCalls != 0 {
		t.Fatalf("exec calls = %d, want 0", fc.execCalls)
	}
}

func TestEvaluateWrongFSType(t *testing.T) {
	cfg := baseConfig()
	fc := &fakeCompose{servicesFn: defaultServicesFn, psFn: runningContainersFn}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts: func() ([]mountinfo.Entry, error) {
			return []mountinfo.Entry{{MountPoint: "/mnt/m", FSType: "ext4"}}, nil
		},
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	if r.MountPresent {
		t.Fatalf("MountPresent = true, want false")
	}
	want := Failure{"mount", "unexpected fstype ext4"}
	if len(r.Failures) == 0 || r.Failures[0] != want {
		t.Fatalf("Failures = %+v, want first %+v", r.Failures, want)
	}
}

func TestEvaluateProbeTimeout(t *testing.T) {
	cfg := baseConfig()
	fc := &fakeCompose{servicesFn: defaultServicesFn, psFn: runningContainersFn}
	sp := &probeSpy{result: probe.Result{Timeout: true, Error: "probe timeout after 10s"}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	var probeFailure *Failure
	for i := range r.Failures {
		if r.Failures[i].Check == "probe" {
			probeFailure = &r.Failures[i]
		}
	}
	if probeFailure == nil || !strings.Contains(probeFailure.Detail, "timeout") {
		t.Fatalf("Failures = %+v, want a probe failure containing timeout", r.Failures)
	}
	if !r.HasMountFailure() {
		t.Fatalf("HasMountFailure() = false, want true")
	}
}

func TestEvaluateContainerNotRunning(t *testing.T) {
	cfg := baseConfig()
	fc := &fakeCompose{
		servicesFn: defaultServicesFn,
		psFn: func(ctx context.Context) ([]compose.Container, error) {
			return []compose.Container{
				{Service: "rclone-mount", State: "exited"},
				{Service: "superdufs", State: "running", Health: "healthy"},
			}, nil
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	if r.Idle {
		t.Fatalf("Idle = true, want false")
	}
	want := Failure{"containers", "rclone-mount: state=exited"}
	found := false
	for _, f := range r.Failures {
		if f == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want to contain %+v", r.Failures, want)
	}
}

func TestEvaluateContainerUnhealthy(t *testing.T) {
	cfg := baseConfig()
	fc := &fakeCompose{
		servicesFn: defaultServicesFn,
		psFn: func(ctx context.Context) ([]compose.Container, error) {
			return []compose.Container{
				{Service: "rclone-mount", State: "running", Health: "unhealthy"},
				{Service: "superdufs", State: "running", Health: "healthy"},
			}, nil
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	want := Failure{"containers", "rclone-mount: health=unhealthy"}
	found := false
	for _, f := range r.Failures {
		if f == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want to contain %+v", r.Failures, want)
	}
}

func TestEvaluateContainerMissing(t *testing.T) {
	cfg := baseConfig()
	fc := &fakeCompose{
		servicesFn: defaultServicesFn,
		psFn: func(ctx context.Context) ([]compose.Container, error) {
			return []compose.Container{
				{Service: "superdufs", State: "running", Health: "healthy"},
			}, nil
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	want := Failure{"containers", "rclone-mount: missing"}
	found := false
	for _, f := range r.Failures {
		if f == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want to contain %+v", r.Failures, want)
	}
}

func TestEvaluateServicesErrorNotIdle(t *testing.T) {
	cfg := baseConfig()
	fc := &fakeCompose{
		servicesFn: func(ctx context.Context) ([]string, error) {
			return nil, errors.New("config broken")
		},
		psFn: func(ctx context.Context) ([]compose.Container, error) {
			return nil, nil
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	if r.Idle {
		t.Fatalf("Idle = true, want false when Services fails")
	}
	if fc.psCalls != 0 {
		t.Fatalf("PS calls = %d, want 0 after Services error", fc.psCalls)
	}
	if len(r.Failures) == 0 || r.Failures[len(r.Failures)-1].Check != "docker" {
		t.Fatalf("Failures = %+v, want a docker failure", r.Failures)
	}
}

func TestEvaluateDockerError(t *testing.T) {
	cfg := baseConfig()
	fc := &fakeCompose{
		servicesFn: defaultServicesFn,
		psFn: func(ctx context.Context) ([]compose.Container, error) {
			return nil, errors.New("boom")
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	if r.Idle {
		t.Fatalf("Idle = true, want false")
	}
	found := false
	for _, f := range r.Failures {
		if f.Check == "docker" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want a docker failure", r.Failures)
	}
}

func TestEvaluateHTTPAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()

	cfg := baseConfig()
	cfg.Checks.HTTP.URL = srv.URL
	fc := &fakeCompose{servicesFn: defaultServicesFn, psFn: runningContainersFn}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
		HTTP:    &http.Client{},
	})
	r := c.Evaluate(context.Background())

	for _, f := range r.Failures {
		if f.Check == "http" {
			t.Fatalf("Failures = %+v, want no http failure", r.Failures)
		}
	}
}

func TestEvaluateHTTPRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	cfg := baseConfig()
	cfg.Checks.HTTP.URL = srv.URL
	fc := &fakeCompose{servicesFn: defaultServicesFn, psFn: runningContainersFn}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
		HTTP:    &http.Client{},
	})
	r := c.Evaluate(context.Background())

	want := Failure{"http", "status 500"}
	found := false
	for _, f := range r.Failures {
		if f == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want to contain %+v", r.Failures, want)
	}
	if r.HasMountFailure() {
		t.Fatalf("HasMountFailure() = true, want false")
	}
}

func TestEvaluateHTTPDisabled(t *testing.T) {
	cfg := baseConfig()
	cfg.Checks.HTTP.URL = ""
	fc := &fakeCompose{servicesFn: defaultServicesFn, psFn: runningContainersFn}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
		HTTP:    nil,
	})
	r := c.Evaluate(context.Background())

	for _, f := range r.Failures {
		if f.Check == "http" {
			t.Fatalf("Failures = %+v, want no http failure", r.Failures)
		}
	}
	if !r.OK {
		t.Fatalf("OK = false, Failures = %+v", r.Failures)
	}
}

func TestEvaluateExecEmptyWhileHostHasEntries(t *testing.T) {
	cfg := baseConfig()
	cfg.Checks.Exec.Service = "rclone-mount"
	cfg.Checks.Exec.Path = "/data"
	fc := &fakeCompose{
		servicesFn: defaultServicesFn,
		psFn:       runningContainersFn,
		execFn: func(ctx context.Context, timeout time.Duration, service string, cmd ...string) ([]byte, error) {
			return []byte(""), nil
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true, HasEntries: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	want := Failure{"exec", "container sees empty directory while host mount has entries"}
	found := false
	for _, f := range r.Failures {
		if f == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want to contain %+v", r.Failures, want)
	}
}

func TestEvaluateExecCanaryMissing(t *testing.T) {
	cfg := baseConfig()
	cfg.Checks.Canary = "c.txt"
	cfg.Checks.Exec.Service = "rclone-mount"
	cfg.Checks.Exec.Path = "/data"
	fc := &fakeCompose{
		servicesFn: defaultServicesFn,
		psFn:       runningContainersFn,
		execFn: func(ctx context.Context, timeout time.Duration, service string, cmd ...string) ([]byte, error) {
			if len(cmd) > 0 && cmd[len(cmd)-1] == "/data/c.txt" {
				return nil, errors.New("no such file or directory")
			}
			return []byte("somefile\n"), nil
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true, HasEntries: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
	})
	r := c.Evaluate(context.Background())

	found := false
	for _, f := range r.Failures {
		if f.Check == "exec" && strings.HasPrefix(f.Detail, "canary: ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want an exec canary failure", r.Failures)
	}
}

func TestServicesCached(t *testing.T) {
	cfg := baseConfig()
	fc := &fakeCompose{servicesFn: defaultServicesFn, psFn: runningContainersFn}
	sp := &probeSpy{result: probe.Result{OK: true}}

	c := New(cfg, Deps{
		Mounts:  mountsOK,
		Probe:   sp.fn,
		Compose: fc,
	})
	c.Evaluate(context.Background())
	c.Evaluate(context.Background())

	if fc.servicesCalls != 1 {
		t.Fatalf("Services calls = %d, want 1", fc.servicesCalls)
	}
}

func TestServicesRefreshedWhenIdle(t *testing.T) {
	cfg := baseConfig()
	ps := runningContainersFn
	fc := &fakeCompose{
		servicesFn: defaultServicesFn,
		psFn: func(ctx context.Context) ([]compose.Container, error) {
			return ps(ctx)
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true}}
	c := New(cfg, Deps{Mounts: mountsOK, Probe: sp.fn, Compose: fc})

	c.Evaluate(context.Background()) // running: services cached
	ps = emptyPSFn
	c.Evaluate(context.Background()) // idle: cache dropped
	ps = runningContainersFn
	c.Evaluate(context.Background()) // running again: services reloaded

	if fc.servicesCalls != 2 {
		t.Fatalf("Services calls = %d, want 2", fc.servicesCalls)
	}
}

func TestServicesRefreshedWhenServiceMissing(t *testing.T) {
	// The compose file renamed rclone-mount to rclone: the cached list still
	// names the old service, which must not stay "missing" forever.
	cfg := baseConfig()
	services := []string{"rclone-mount", "superdufs"}
	fc := &fakeCompose{
		servicesFn: func(ctx context.Context) ([]string, error) { return services, nil },
		psFn: func(ctx context.Context) ([]compose.Container, error) {
			return []compose.Container{
				{Name: "r", Service: "rclone", State: "running"},
				{Name: "s", Service: "superdufs", State: "running"},
			}, nil
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true}}
	c := New(cfg, Deps{Mounts: mountsOK, Probe: sp.fn, Compose: fc})

	r := c.Evaluate(context.Background())
	if r.OK {
		t.Fatalf("OK = true on first evaluation, want rclone-mount missing")
	}
	services = []string{"rclone", "superdufs"}
	r = c.Evaluate(context.Background())
	if !r.OK {
		t.Fatalf("OK = false after services reload, Failures = %+v", r.Failures)
	}
	if fc.servicesCalls != 2 {
		t.Fatalf("Services calls = %d, want 2", fc.servicesCalls)
	}
}

func TestEvaluateExecUsesContainerName(t *testing.T) {
	cfg := baseConfig()
	cfg.Checks.Exec.Service = "superdufs"
	cfg.Checks.Exec.Path = "/data"
	var targets []string
	fc := &fakeCompose{
		servicesFn: defaultServicesFn,
		psFn:       runningContainersFn,
		execFn: func(ctx context.Context, timeout time.Duration, container string, cmd ...string) ([]byte, error) {
			targets = append(targets, container)
			return []byte("somefile\n"), nil
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true, HasEntries: true}}
	c := New(cfg, Deps{Mounts: mountsOK, Probe: sp.fn, Compose: fc})

	r := c.Evaluate(context.Background())
	if !r.OK {
		t.Fatalf("OK = false, Failures = %+v", r.Failures)
	}
	if len(targets) != 1 || targets[0] != "m-superdufs-1" {
		t.Fatalf("exec targets = %v, want [m-superdufs-1]", targets)
	}
}

func TestEvaluateExecNoRunningContainer(t *testing.T) {
	cfg := baseConfig()
	cfg.Checks.Exec.Service = "superdufs"
	cfg.Checks.Exec.Path = "/data"
	fc := &fakeCompose{
		servicesFn: defaultServicesFn,
		psFn: func(ctx context.Context) ([]compose.Container, error) {
			return []compose.Container{
				{Name: "m-rclone-mount-1", Service: "rclone-mount", State: "running"},
				{Name: "m-superdufs-1", Service: "superdufs", State: "exited"},
			}, nil
		},
	}
	sp := &probeSpy{result: probe.Result{OK: true}}
	c := New(cfg, Deps{Mounts: mountsOK, Probe: sp.fn, Compose: fc})

	r := c.Evaluate(context.Background())
	want := Failure{"exec", "superdufs: no running container"}
	found := false
	for _, f := range r.Failures {
		if f == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("Failures = %+v, want to contain %+v", r.Failures, want)
	}
	if fc.execCalls != 0 {
		t.Fatalf("exec calls = %d, want 0", fc.execCalls)
	}
}

func TestEvaluateExecSkippedWhenProbeFails(t *testing.T) {
	cfg := baseConfig()
	cfg.Checks.Exec.Service = "superdufs"
	cfg.Checks.Exec.Path = "/data"
	fc := &fakeCompose{servicesFn: defaultServicesFn, psFn: runningContainersFn}
	sp := &probeSpy{result: probe.Result{Error: "transport endpoint is not connected"}}
	c := New(cfg, Deps{Mounts: mountsOK, Probe: sp.fn, Compose: fc})

	r := c.Evaluate(context.Background())
	if !r.HasMountFailure() {
		t.Fatalf("Failures = %+v, want a probe failure", r.Failures)
	}
	if fc.execCalls != 0 {
		t.Fatalf("exec calls = %d, want 0 when the probe fails", fc.execCalls)
	}
}

func TestReportDockerUnavailable(t *testing.T) {
	if (Report{Failures: []Failure{{"http", "status 500"}}}).DockerUnavailable() {
		t.Fatalf("DockerUnavailable() = true without a docker failure")
	}
	r := Report{Failures: []Failure{{"mount", "not mounted"}, {"docker", "boom"}}}
	if !r.DockerUnavailable() {
		t.Fatalf("DockerUnavailable() = false with a docker failure")
	}
}

func TestReportSummary(t *testing.T) {
	ok := Report{OK: true}
	if ok.Summary() != "ok" {
		t.Fatalf("Summary() = %q, want ok", ok.Summary())
	}

	idle := Report{Idle: true, Failures: []Failure{{"containers", "rclone-mount: missing"}}}
	if idle.Summary() != "idle" {
		t.Fatalf("Summary() = %q, want idle", idle.Summary())
	}

	failing := Report{Failures: []Failure{
		{"mount", "not mounted"},
		{"docker", "boom"},
	}}
	want := "mount: not mounted; docker: boom"
	if failing.Summary() != want {
		t.Fatalf("Summary() = %q, want %q", failing.Summary(), want)
	}
	if !failing.HasMountFailure() {
		t.Fatalf("HasMountFailure() = false, want true")
	}
}

func TestEvaluateStartingOnlyForRunningContainers(t *testing.T) {
	cases := []struct {
		state, health string
		want          bool
	}{
		{"running", "starting", true},
		{"paused", "starting", false},
		{"running", "unhealthy", false},
		{"running", "healthy", false},
	}
	for _, tc := range cases {
		t.Run(tc.state+"/"+tc.health, func(t *testing.T) {
			fc := &fakeCompose{
				servicesFn: defaultServicesFn,
				psFn: func(ctx context.Context) ([]compose.Container, error) {
					return []compose.Container{
						{Service: "rclone-mount", State: tc.state, Health: tc.health},
						{Service: "superdufs", State: "created"},
					}, nil
				},
			}
			c := New(baseConfig(), Deps{Mounts: mountsMissing, Probe: (&probeSpy{}).fn, Compose: fc})
			r := c.Evaluate(context.Background())
			if r.Starting != tc.want {
				t.Fatalf("Starting = %v, want %v (report %+v)", r.Starting, tc.want, r)
			}
			if r.OK {
				t.Fatalf("OK = true, want false")
			}
		})
	}
}
