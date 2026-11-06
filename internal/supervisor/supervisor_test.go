package supervisor

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"mount-manager/internal/config"
	"mount-manager/internal/health"
	"mount-manager/internal/notify"
)

// --- fakes ---

type fakeHealth struct {
	reports []health.Report
	idx     int
	calls   int
}

func (f *fakeHealth) Evaluate(ctx context.Context) health.Report {
	f.calls++
	if len(f.reports) == 0 {
		return health.Report{}
	}
	if f.idx >= len(f.reports) {
		return f.reports[len(f.reports)-1]
	}
	r := f.reports[f.idx]
	f.idx++
	return r
}

type recoverCall struct {
	attempt    int
	abortFirst bool
}

type fakeRecovery struct {
	calls        []recoverCall
	errs         []error
	errIdx       int
	cleanupCalls []bool
	cleanupErr   error
}

func (f *fakeRecovery) Recover(ctx context.Context, attempt int, abortFirst bool) error {
	f.calls = append(f.calls, recoverCall{attempt: attempt, abortFirst: abortFirst})
	if len(f.errs) == 0 {
		return nil
	}
	var err error
	if f.errIdx < len(f.errs) {
		err = f.errs[f.errIdx]
		f.errIdx++
	} else {
		err = f.errs[len(f.errs)-1]
	}
	return err
}

func (f *fakeRecovery) CleanupMount(ctx context.Context, abort bool) error {
	f.cleanupCalls = append(f.cleanupCalls, abort)
	return f.cleanupErr
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type recNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *recNotifier) Notify(e notify.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, e)
}

func (n *recNotifier) all() []notify.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]notify.Event, len(n.events))
	copy(out, n.events)
	return out
}

// --- helpers ---

var baseTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func testConfig() *config.Config {
	return &config.Config{
		MountPath:        "/mnt/m",
		ExpectedFSType:   "fuse.rclone",
		CheckInterval:    config.Duration{Duration: 10 * time.Second},
		FailureThreshold: 3,
		StartupGrace:     config.Duration{Duration: 120 * time.Second},
		Recovery: config.Recovery{
			VerifyTimeout:  config.Duration{Duration: 120 * time.Second},
			BackoffInitial: config.Duration{Duration: 10 * time.Second},
			BackoffMax:     config.Duration{Duration: 5 * time.Minute},
		},
	}
}

func newSup(cfg *config.Config, h Health, rec Recovery, clk Clock, n notify.Notifier) *Supervisor {
	return New(Options{
		Config:   cfg,
		Health:   h,
		Recovery: rec,
		Notifier: n,
		Clock:    clk,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Host:     "host1",
		Version:  "1.0.0",
	})
}

func okReport() health.Report   { return health.Report{OK: true} }
func idleReport() health.Report { return health.Report{Idle: true} }
func staleReport() health.Report {
	return health.Report{Idle: true, MountPresent: true, MountStale: true}
}
func probeFailReport() health.Report {
	return health.Report{Failures: []health.Failure{{Check: "probe", Detail: "timeout"}}}
}
func httpFailReport() health.Report {
	return health.Report{Failures: []health.Failure{{Check: "http", Detail: "status 500"}}}
}

func eventNames(events []notify.Event) []string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.Event
	}
	return names
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- tests ---

func TestStartEmitsStartupAndWaiting(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	sup := newSup(testConfig(), &fakeHealth{}, &fakeRecovery{}, clk, n)

	sup.Start()

	got := eventNames(n.all())
	want := []string{"startup", "waiting"}
	if !sliceEq(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if sup.State() != Waiting {
		t.Fatalf("state = %v, want Waiting", sup.State())
	}
}

func TestWaitingIdleStaysWaiting(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{idleReport()}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()

	for i := 0; i < 3; i++ {
		d := sup.Step(context.Background())
		if d != 10*time.Second {
			t.Fatalf("step %d: duration = %v, want 10s", i, d)
		}
	}

	if len(rec.calls) != 0 {
		t.Fatalf("Recover calls = %d, want 0", len(rec.calls))
	}
	if len(rec.cleanupCalls) != 0 {
		t.Fatalf("CleanupMount calls = %d, want 0", len(rec.cleanupCalls))
	}
	if len(n.all()) != 2 {
		t.Fatalf("events = %v, want only startup+waiting", n.all())
	}
	if sup.State() != Waiting {
		t.Fatalf("state = %v, want Waiting", sup.State())
	}
}

func TestWaitingToMonitoringOnOK(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{okReport()}}
	sup := newSup(testConfig(), h, &fakeRecovery{}, clk, n)
	sup.Start()

	sup.Step(context.Background())

	got := eventNames(n.all())
	want := []string{"startup", "waiting", "stack_up"}
	if !sliceEq(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if sup.State() != Monitoring {
		t.Fatalf("state = %v, want Monitoring", sup.State())
	}
}

func TestWaitingStaleMountCleaned(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{staleReport()}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()

	sup.Step(context.Background())

	if len(rec.cleanupCalls) != 1 || rec.cleanupCalls[0] != true {
		t.Fatalf("cleanupCalls = %v, want [true]", rec.cleanupCalls)
	}
	got := eventNames(n.all())
	want := []string{"startup", "waiting", "stale_mount_cleaned"}
	if !sliceEq(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}

	// with cleanup error: no event.
	clk2 := &fakeClock{now: baseTime}
	n2 := &recNotifier{}
	h2 := &fakeHealth{reports: []health.Report{staleReport()}}
	rec2 := &fakeRecovery{cleanupErr: errFake("boom")}
	sup2 := newSup(testConfig(), h2, rec2, clk2, n2)
	sup2.Start()
	sup2.Step(context.Background())

	got2 := eventNames(n2.all())
	want2 := []string{"startup", "waiting"}
	if !sliceEq(got2, want2) {
		t.Fatalf("events = %v, want %v (no stale_mount_cleaned on error)", got2, want2)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestWaitingGraceThenRecovery(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{probeFailReport()}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()

	sup.Step(context.Background()) // t0: grace starts
	if len(rec.calls) != 0 {
		t.Fatalf("Recover called too early")
	}

	clk.now = baseTime.Add(119 * time.Second)
	sup.Step(context.Background())
	if len(rec.calls) != 0 {
		t.Fatalf("Recover called at 119s, want none")
	}

	clk.now = baseTime.Add(120 * time.Second)
	sup.Step(context.Background())

	got := eventNames(n.all())
	want := []string{"startup", "waiting", "failure_detected", "recovery_started"}
	if !sliceEq(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if len(rec.calls) != 1 || rec.calls[0] != (recoverCall{attempt: 1, abortFirst: true}) {
		t.Fatalf("recover calls = %v, want [{1 true}]", rec.calls)
	}
	if sup.State() != Verifying {
		t.Fatalf("state = %v, want Verifying", sup.State())
	}
}

func TestWaitingGraceResetsWhenIdle(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{probeFailReport(), idleReport(), probeFailReport()}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()

	sup.Step(context.Background()) // t0: probeFail, grace starts at t0

	clk.now = baseTime.Add(60 * time.Second)
	sup.Step(context.Background()) // idle: grace resets

	clk.now = baseTime.Add(100 * time.Second)
	sup.Step(context.Background()) // probeFail: grace starts at 100s

	clk.now = baseTime.Add(200 * time.Second)
	sup.Step(context.Background()) // 100s since grace start: no recovery yet
	if len(rec.calls) != 0 {
		t.Fatalf("Recover called too early at 200s")
	}

	clk.now = baseTime.Add(220 * time.Second)
	sup.Step(context.Background()) // 120s since grace start: recovery
	if len(rec.calls) != 1 {
		t.Fatalf("Recover calls = %d, want 1", len(rec.calls))
	}
}

func TestMonitoringThresholdTriggersRecovery(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{
		okReport(),
		httpFailReport(),
		httpFailReport(),
		okReport(),
		httpFailReport(),
		httpFailReport(),
		httpFailReport(),
	}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()

	sup.Step(context.Background()) // ok -> monitoring
	if sup.State() != Monitoring {
		t.Fatalf("state = %v, want Monitoring", sup.State())
	}
	sup.Step(context.Background()) // fail 1
	sup.Step(context.Background()) // fail 2
	sup.Step(context.Background()) // ok -> reset
	sup.Step(context.Background()) // fail 1
	sup.Step(context.Background()) // fail 2
	if len(rec.calls) != 0 {
		t.Fatalf("Recover called too early: %v", rec.calls)
	}
	sup.Step(context.Background()) // fail 3 -> recover
	if len(rec.calls) != 1 {
		t.Fatalf("Recover calls = %d, want 1", len(rec.calls))
	}
}

func TestMonitoringIdleGoesWaiting(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{okReport(), idleReport()}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()

	sup.Step(context.Background()) // ok -> monitoring
	sup.Step(context.Background()) // idle -> waiting

	if sup.State() != Waiting {
		t.Fatalf("state = %v, want Waiting", sup.State())
	}
	if len(rec.calls) != 0 {
		t.Fatalf("Recover calls = %d, want 0", len(rec.calls))
	}
	got := eventNames(n.all())
	want := []string{"startup", "waiting", "stack_up", "stack_stopped"}
	if !sliceEq(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRecoveryAbortFirstFlag(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{
		okReport(), probeFailReport(), probeFailReport(), probeFailReport(),
	}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()
	for i := 0; i < 4; i++ {
		sup.Step(context.Background())
	}
	if len(rec.calls) != 1 || rec.calls[0].abortFirst != true {
		t.Fatalf("calls = %v, want abortFirst=true", rec.calls)
	}

	clk2 := &fakeClock{now: baseTime}
	n2 := &recNotifier{}
	h2 := &fakeHealth{reports: []health.Report{
		okReport(), httpFailReport(), httpFailReport(), httpFailReport(),
	}}
	rec2 := &fakeRecovery{}
	sup2 := newSup(testConfig(), h2, rec2, clk2, n2)
	sup2.Start()
	for i := 0; i < 4; i++ {
		sup2.Step(context.Background())
	}
	if len(rec2.calls) != 1 || rec2.calls[0].abortFirst != false {
		t.Fatalf("calls = %v, want abortFirst=false", rec2.calls)
	}
}

func TestVerifyingSuccess(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{
		okReport(),       // -> monitoring
		httpFailReport(), // fail 1
		httpFailReport(), // fail 2
		httpFailReport(), // fail 3 -> recover attempt 1, verifying
		okReport(),       // verify success -> monitoring, attempt reset
		httpFailReport(), // fail 1
		httpFailReport(), // fail 2
		httpFailReport(), // fail 3 -> recover attempt 1 again
	}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()
	for i := 0; i < 4; i++ {
		sup.Step(context.Background())
	}
	if sup.State() != Verifying {
		t.Fatalf("state = %v, want Verifying", sup.State())
	}
	sup.Step(context.Background()) // verify ok
	if sup.State() != Monitoring {
		t.Fatalf("state = %v, want Monitoring", sup.State())
	}
	events := n.all()
	if events[len(events)-1].Event != notify.EventRecoverySucceeded || events[len(events)-1].Attempt != 1 {
		t.Fatalf("last event = %+v, want recovery_succeeded attempt 1", events[len(events)-1])
	}

	for i := 0; i < 3; i++ {
		sup.Step(context.Background())
	}
	if len(rec.calls) != 2 {
		t.Fatalf("Recover calls = %d, want 2", len(rec.calls))
	}
	if rec.calls[1].attempt != 1 {
		t.Fatalf("second recovery attempt = %d, want 1", rec.calls[1].attempt)
	}
}

func TestVerifyingIdleGoesWaiting(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{
		okReport(), httpFailReport(), httpFailReport(), httpFailReport(), idleReport(), idleReport(),
	}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()
	for i := 0; i < 4; i++ {
		sup.Step(context.Background())
	}
	if sup.State() != Verifying {
		t.Fatalf("state = %v, want Verifying", sup.State())
	}
	sup.Step(context.Background()) // stopped by hand while verifying
	if sup.State() != Waiting {
		t.Fatalf("state = %v, want Waiting", sup.State())
	}
	events := n.all()
	last := events[len(events)-1]
	if last.Event != notify.EventStackStopped || last.Attempt != 1 {
		t.Fatalf("last event = %+v, want stack_stopped attempt 1", last)
	}
	if sup.attempt != 0 {
		t.Fatalf("attempt = %d, want 0", sup.attempt)
	}
	clk.now = baseTime.Add(time.Hour)
	sup.Step(context.Background()) // idle long past verify_timeout: no recovery (I-1)
	if len(rec.calls) != 1 || sup.State() != Waiting {
		t.Fatalf("recover calls = %d, state = %v; want 1, Waiting", len(rec.calls), sup.State())
	}
}

func TestVerifyingTimeoutBackoff(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{
		okReport(), httpFailReport(), httpFailReport(), httpFailReport(),
	}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()
	for i := 0; i < 4; i++ {
		sup.Step(context.Background())
	}
	if sup.State() != Verifying {
		t.Fatalf("state = %v, want Verifying", sup.State())
	}
	// verifyDeadline = t0 + 120s; fail until deadline
	clk.now = baseTime.Add(120 * time.Second)
	d := sup.Step(context.Background())
	if d != 10*time.Second {
		t.Fatalf("duration = %v, want 10s", d)
	}
	if sup.State() != StateBackoff {
		t.Fatalf("state = %v, want StateBackoff", sup.State())
	}
	events := n.all()
	last := events[len(events)-1]
	if last.Event != notify.EventRecoveryFailed {
		t.Fatalf("last event = %+v, want recovery_failed", last)
	}

	callsBefore := h.calls
	clk.now = baseTime.Add(125 * time.Second) // +5s
	d = sup.Step(context.Background())
	if d != 5*time.Second {
		t.Fatalf("duration = %v, want 5s", d)
	}
	if h.calls != callsBefore {
		t.Fatalf("Evaluate called during backoff wait")
	}
	if len(rec.calls) != 1 {
		t.Fatalf("Recover called during backoff wait")
	}

	clk.now = baseTime.Add(130 * time.Second) // +10s
	sup.Step(context.Background())
	if len(rec.calls) != 2 {
		t.Fatalf("Recover calls = %d, want 2", len(rec.calls))
	}
	if rec.calls[1].attempt != 2 || rec.calls[1].abortFirst != true {
		t.Fatalf("second recover = %+v, want {2 true}", rec.calls[1])
	}
}

func TestRecoverErrorGoesBackoff(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{
		okReport(), httpFailReport(), httpFailReport(), httpFailReport(),
	}}
	rec := &fakeRecovery{errs: []error{errFake("recover failed")}}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()
	for i := 0; i < 4; i++ {
		sup.Step(context.Background())
	}
	if sup.State() != StateBackoff {
		t.Fatalf("state = %v, want StateBackoff", sup.State())
	}
	events := n.all()
	last := events[len(events)-1]
	if last.Event != notify.EventRecoveryFailed || last.Detail != "recover failed" {
		t.Fatalf("last event = %+v, want recovery_failed 'recover failed'", last)
	}

	clk.now = baseTime.Add(10 * time.Second)
	sup.Step(context.Background()) // triggers retry (attempt 2), fails again
	if sup.State() != StateBackoff {
		t.Fatalf("state = %v, want StateBackoff", sup.State())
	}
	if len(rec.calls) != 2 || rec.calls[1].attempt != 2 {
		t.Fatalf("calls = %v, want 2nd attempt=2", rec.calls)
	}
	// backoff for attempt 2 should be 20s.
	wantUntil := baseTime.Add(10 * time.Second).Add(20 * time.Second)
	if !sup.backoffUntil.Equal(wantUntil) {
		t.Fatalf("backoffUntil = %v, want %v", sup.backoffUntil, wantUntil)
	}
}

func dockerDownReport() health.Report {
	return health.Report{Failures: []health.Failure{
		{Check: "mount", Detail: "not mounted"},
		{Check: "docker", Detail: "Cannot connect to the Docker daemon"},
	}}
}

// backoffAfterFailedRecovery drives a supervisor through ok -> 3 http
// failures -> failing Recover, leaving it in StateBackoff at baseTime with
// backoffUntil = baseTime+10s. Further reports come from next.
func backoffAfterFailedRecovery(t *testing.T, next ...health.Report) (*Supervisor, *fakeRecovery, *fakeClock, *recNotifier) {
	t.Helper()
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	reports := append([]health.Report{okReport(), httpFailReport(), httpFailReport(), httpFailReport()}, next...)
	h := &fakeHealth{reports: reports}
	rec := &fakeRecovery{errs: []error{errFake("recover failed"), nil}}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()
	for i := 0; i < 4; i++ {
		sup.Step(context.Background())
	}
	if sup.State() != StateBackoff {
		t.Fatalf("state = %v, want StateBackoff", sup.State())
	}
	return sup, rec, clk, n
}

func TestWaitingDockerUnavailableNoRecovery(t *testing.T) {
	// Stack intentionally stopped, then the docker daemon goes away for
	// longer than startup_grace: the stack must not be brought up (I-1).
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{idleReport(), dockerDownReport()}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()

	for i := 0; i < 60; i++ {
		sup.Step(context.Background())
		clk.now = clk.now.Add(10 * time.Second)
	}
	if len(rec.calls) != 0 || len(rec.cleanupCalls) != 0 {
		t.Fatalf("recover calls = %v, cleanup calls = %v, want none", rec.calls, rec.cleanupCalls)
	}
	if sup.State() != Waiting {
		t.Fatalf("state = %v, want Waiting", sup.State())
	}
	if got := eventNames(n.all()); !sliceEq(got, []string{"startup", "waiting"}) {
		t.Fatalf("events = %v, want only startup+waiting", got)
	}
}

func TestWaitingDockerUnavailableResetsGrace(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{probeFailReport(), dockerDownReport(), probeFailReport()}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()

	sup.Step(context.Background()) // t0: grace starts
	clk.now = baseTime.Add(60 * time.Second)
	sup.Step(context.Background()) // docker down: grace reset
	clk.now = baseTime.Add(100 * time.Second)
	sup.Step(context.Background()) // grace restarts at 100s
	clk.now = baseTime.Add(200 * time.Second)
	sup.Step(context.Background())
	if len(rec.calls) != 0 {
		t.Fatalf("Recover called at 200s, grace must restart after the docker outage")
	}
	clk.now = baseTime.Add(220 * time.Second)
	sup.Step(context.Background())
	if len(rec.calls) != 1 {
		t.Fatalf("Recover calls = %d, want 1", len(rec.calls))
	}
}

func TestMonitoringDockerUnavailableNoRecovery(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{
		okReport(),
		dockerDownReport(), dockerDownReport(), dockerDownReport(), dockerDownReport(),
		httpFailReport(), httpFailReport(), // 2 real failures: below threshold
		httpFailReport(), // third real failure: recovery
	}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()

	for i := 0; i < 7; i++ {
		sup.Step(context.Background())
	}
	if len(rec.calls) != 0 {
		t.Fatalf("Recover calls = %v, docker outages must not count toward the threshold", rec.calls)
	}
	if sup.State() != Monitoring {
		t.Fatalf("state = %v, want Monitoring", sup.State())
	}
	sup.Step(context.Background())
	if len(rec.calls) != 1 {
		t.Fatalf("Recover calls = %d, want 1", len(rec.calls))
	}
}

func TestBackoffHoldsWhileDockerUnavailable(t *testing.T) {
	sup, rec, clk, _ := backoffAfterFailedRecovery(t, dockerDownReport(), dockerDownReport(), httpFailReport())

	clk.now = baseTime.Add(10 * time.Second)
	if d := sup.Step(context.Background()); d != 10*time.Second {
		t.Fatalf("duration = %v, want check interval 10s", d)
	}
	clk.now = baseTime.Add(20 * time.Second)
	sup.Step(context.Background())
	if len(rec.calls) != 1 {
		t.Fatalf("Recover calls = %d, want 1 (hold while docker is unavailable)", len(rec.calls))
	}
	if sup.State() != StateBackoff {
		t.Fatalf("state = %v, want StateBackoff", sup.State())
	}

	clk.now = baseTime.Add(30 * time.Second)
	sup.Step(context.Background()) // docker back, still failing: retry
	if len(rec.calls) != 2 || rec.calls[1].attempt != 2 {
		t.Fatalf("calls = %v, want a second attempt numbered 2", rec.calls)
	}
}

func TestBackoffOKGoesMonitoring(t *testing.T) {
	sup, rec, clk, n := backoffAfterFailedRecovery(t, okReport())

	clk.now = baseTime.Add(10 * time.Second)
	sup.Step(context.Background())
	if sup.State() != Monitoring {
		t.Fatalf("state = %v, want Monitoring", sup.State())
	}
	if len(rec.calls) != 1 {
		t.Fatalf("Recover calls = %d, want 1 (no retry when already healthy)", len(rec.calls))
	}
	events := n.all()
	last := events[len(events)-1]
	if last.Event != notify.EventRecoverySucceeded || last.Attempt != 1 {
		t.Fatalf("last event = %+v, want recovery_succeeded attempt 1", last)
	}
}

func TestBackoffIdleStillRetries(t *testing.T) {
	// A failed up can leave nothing running: idle in backoff still retries.
	sup, rec, clk, _ := backoffAfterFailedRecovery(t, idleReport())

	clk.now = baseTime.Add(10 * time.Second)
	sup.Step(context.Background())
	if len(rec.calls) != 2 {
		t.Fatalf("Recover calls = %d, want 2", len(rec.calls))
	}
	if sup.State() != Verifying {
		t.Fatalf("state = %v, want Verifying", sup.State())
	}
}

func TestEventFields(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	cfg := testConfig()
	h := &fakeHealth{reports: []health.Report{okReport()}}
	sup := New(Options{
		Config:   cfg,
		Health:   h,
		Recovery: &fakeRecovery{},
		Notifier: n,
		Clock:    clk,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Host:     "h1",
		Version:  "1.0.0",
	})
	sup.Start()
	for _, ev := range n.all() {
		if ev.Host != "h1" || ev.MountPath != "/mnt/m" || !ev.Timestamp.Equal(baseTime) {
			t.Fatalf("event %+v, want Host h1, MountPath /mnt/m, Timestamp %v", ev, baseTime)
		}
	}

	clk.now = baseTime.Add(10 * time.Second)
	sup.Step(context.Background())
	events := n.all()
	last := events[len(events)-1]
	if last.Event != notify.EventStackUp || !last.Timestamp.Equal(baseTime.Add(10*time.Second)) {
		t.Fatalf("last event = %+v, want stack_up at T+10s", last)
	}
	if last.Host != "h1" || last.MountPath != "/mnt/m" {
		t.Fatalf("last event = %+v, want Host h1, MountPath /mnt/m", last)
	}
}

func TestRunShutdownDuringRecoveryWarns(t *testing.T) {
	cfg := testConfig()
	cfg.CheckInterval = config.Duration{Duration: 10 * time.Millisecond}
	cfg.StartupGrace = config.Duration{Duration: time.Nanosecond}
	cfg.Recovery.BackoffInitial = config.Duration{Duration: time.Hour}
	cfg.Recovery.BackoffMax = config.Duration{Duration: time.Hour}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{probeFailReport()}}
	rec := &fakeRecovery{errs: []error{errFake("compose up failed")}}
	sup := newSup(cfg, h, rec, RealClock{}, n)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	deadline := time.Now().Add(time.Second)
	for {
		evs := n.all()
		if len(evs) > 0 && evs[len(evs)-1].Event == notify.EventRecoveryFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no recovery_failed within 1s: %v", eventNames(evs))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	events := n.all()
	last := events[len(events)-1]
	if last.Event != notify.EventShutdown || last.Attempt != 1 || !strings.Contains(last.Detail, "during recovery") {
		t.Fatalf("last event = %+v, want shutdown during recovery, attempt 1", last)
	}
}

func TestBackoffSequence(t *testing.T) {
	initial := 10 * time.Second
	max := 5 * time.Minute
	want := []time.Duration{
		10 * time.Second,  // n=0
		10 * time.Second,  // n=1
		20 * time.Second,  // n=2
		40 * time.Second,  // n=3
		80 * time.Second,  // n=4
		160 * time.Second, // n=5
		5 * time.Minute,   // n=6
		5 * time.Minute,   // n=7
		5 * time.Minute,   // n=8
	}
	for n, w := range want {
		got := Backoff(n, initial, max)
		if got != w {
			t.Errorf("Backoff(%d) = %v, want %v", n, got, w)
		}
	}
	if got := Backoff(1000, initial, max); got != max {
		t.Errorf("Backoff(1000) = %v, want %v", got, max)
	}
	if got := Backoff(3, max, max); got != max {
		t.Errorf("Backoff(3, max, max) = %v, want %v", got, max)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	cfg := testConfig()
	cfg.CheckInterval = config.Duration{Duration: 10 * time.Millisecond}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{okReport()}}
	rec := &fakeRecovery{}
	sup := newSup(cfg, h, rec, RealClock{}, n)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		done <- sup.Run(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return within 1s")
	}

	events := n.all()
	if len(events) == 0 || events[len(events)-1].Event != notify.EventShutdown {
		t.Fatalf("last event = %v, want shutdown", events)
	}
}

// cancelingHealth returns report after canceling ctx, as when a signal
// arrives while Evaluate is running: probes report "probe canceled" and the
// HTTP check errors, so the report is false.
type cancelingHealth struct {
	report health.Report
	cancel context.CancelFunc
	calls  int
}

func (h *cancelingHealth) Evaluate(ctx context.Context) health.Report {
	h.calls++
	h.cancel()
	return h.report
}

func TestCanceledEvaluateTakesNoAction(t *testing.T) {
	cases := []struct {
		name  string
		setup func(sup *Supervisor, clk *fakeClock)
		rep   health.Report
	}{
		{"waiting idle stale", func(*Supervisor, *fakeClock) {}, staleReport()},
		{"waiting grace expired", func(sup *Supervisor, clk *fakeClock) {
			sup.graceStart = baseTime.Add(-time.Hour)
		}, probeFailReport()},
		{"monitoring at threshold", func(sup *Supervisor, clk *fakeClock) {
			sup.state = Monitoring
			sup.failCount = 2
		}, probeFailReport()},
		{"verifying deadline passed", func(sup *Supervisor, clk *fakeClock) {
			sup.state = Verifying
			sup.attempt = 1
			sup.verifyDeadline = baseTime.Add(-time.Second)
		}, probeFailReport()},
		{"backoff expired", func(sup *Supervisor, clk *fakeClock) {
			sup.state = StateBackoff
			sup.attempt = 1
			sup.backoffUntil = baseTime.Add(-time.Second)
		}, probeFailReport()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := &fakeClock{now: baseTime}
			n := &recNotifier{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := &cancelingHealth{report: tc.rep, cancel: cancel}
			rec := &fakeRecovery{}
			sup := newSup(testConfig(), h, rec, clk, n)
			sup.Start()
			tc.setup(sup, clk)
			before := sup.State()

			sup.Step(ctx)

			if h.calls != 1 {
				t.Fatalf("Evaluate calls = %d, want 1", h.calls)
			}
			if len(rec.calls) != 0 || len(rec.cleanupCalls) != 0 {
				t.Fatalf("Recover calls = %v, CleanupMount calls = %v, want none", rec.calls, rec.cleanupCalls)
			}
			if got := eventNames(n.all()); !sliceEq(got, []string{"startup", "waiting"}) {
				t.Fatalf("events = %v, want only startup+waiting", got)
			}
			if sup.State() != before {
				t.Fatalf("state = %v, want unchanged %v", sup.State(), before)
			}
		})
	}
}

// cancelingRecovery cancels ctx during Recover and returns the context error,
// as when a signal interrupts the compose commands.
type cancelingRecovery struct {
	fakeRecovery
	cancel context.CancelFunc
}

func (r *cancelingRecovery) Recover(ctx context.Context, attempt int, abortFirst bool) error {
	r.fakeRecovery.calls = append(r.fakeRecovery.calls, recoverCall{attempt, abortFirst})
	r.cancel()
	return ctx.Err()
}

func TestRecoveryInterruptedByShutdownIsNotAFailure(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &fakeHealth{reports: []health.Report{probeFailReport()}}
	rec := &cancelingRecovery{cancel: cancel}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()
	sup.state = Monitoring
	sup.failCount = 2

	sup.Step(ctx)

	got := eventNames(n.all())
	want := []string{"startup", "waiting", "failure_detected", "recovery_started"}
	if !sliceEq(got, want) {
		t.Fatalf("events = %v, want %v (no recovery_failed on shutdown)", got, want)
	}
	if sup.State() != StateBackoff {
		t.Fatalf("state = %v, want backoff (recovery interrupted)", sup.State())
	}
}

// lockedBuffer is an io.Writer safe for the slog handler.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

func TestStaleCleanupErrorLoggedOnChangeOnly(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{staleReport()}}
	rec := &fakeRecovery{cleanupErr: errFake("permission denied")}
	logs := &lockedBuffer{}
	sup := New(Options{
		Config: testConfig(), Health: h, Recovery: rec, Notifier: n, Clock: clk,
		Log:  slog.New(slog.NewTextHandler(logs, nil)),
		Host: "host1", Version: "1.0.0",
	})
	sup.Start()

	for i := 0; i < 5; i++ {
		sup.Step(context.Background())
	}
	if len(rec.cleanupCalls) != 5 {
		t.Fatalf("CleanupMount calls = %d, want 5 (retried every check)", len(rec.cleanupCalls))
	}
	if c := logs.count("stale mount cleanup failed"); c != 1 {
		t.Fatalf("error logged %d times, want 1", c)
	}

	rec.cleanupErr = errFake("other error")
	sup.Step(context.Background())
	if c := logs.count("stale mount cleanup failed"); c != 2 {
		t.Fatalf("error logged %d times after the error changed, want 2", c)
	}

	rec.cleanupErr = nil
	sup.Step(context.Background())
	rec.cleanupErr = errFake("other error")
	sup.Step(context.Background())
	if c := logs.count("stale mount cleanup failed"); c != 3 {
		t.Fatalf("error logged %d times after a success in between, want 3", c)
	}
}

func startingReport() health.Report {
	return health.Report{Starting: true, Failures: []health.Failure{{Check: "containers", Detail: "rclone-mount: health=starting"}}}
}

func TestWaitingGraceHeldWhileStarting(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{
		startingReport(), startingReport(), probeFailReport(), probeFailReport(), probeFailReport(),
	}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()

	for _, at := range []time.Duration{0, 200 * time.Second} {
		clk.now = baseTime.Add(at)
		sup.Step(context.Background())
	}
	if len(rec.calls) != 0 {
		t.Fatalf("Recover called while a container is starting")
	}

	clk.now = baseTime.Add(201 * time.Second) // starting over: grace begins
	sup.Step(context.Background())
	clk.now = baseTime.Add(320 * time.Second)
	sup.Step(context.Background())
	if len(rec.calls) != 0 {
		t.Fatalf("Recover called at 119s of grace, want none")
	}
	clk.now = baseTime.Add(321 * time.Second)
	sup.Step(context.Background())
	if len(rec.calls) != 1 {
		t.Fatalf("Recover calls = %d, want 1 after the grace", len(rec.calls))
	}
}

func TestMonitoringStartingNotCounted(t *testing.T) {
	clk := &fakeClock{now: baseTime}
	n := &recNotifier{}
	h := &fakeHealth{reports: []health.Report{
		okReport(), httpFailReport(),
		startingReport(), startingReport(), startingReport(), startingReport(),
		httpFailReport(), httpFailReport(),
	}}
	rec := &fakeRecovery{}
	sup := newSup(testConfig(), h, rec, clk, n)
	sup.Start()
	for i := 0; i < 7; i++ {
		sup.Step(context.Background())
	}
	if len(rec.calls) != 0 || sup.State() != Monitoring {
		t.Fatalf("recover calls = %d, state = %v; want 0, Monitoring", len(rec.calls), sup.State())
	}
	sup.Step(context.Background()) // third counted failure
	if len(rec.calls) != 1 {
		t.Fatalf("Recover calls = %d, want 1", len(rec.calls))
	}
}
