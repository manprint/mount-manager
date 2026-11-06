package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"mount-manager/internal/config"
	"mount-manager/internal/mountinfo"
)

var (
	fuseEntry  = mountinfo.Entry{MountPoint: "/mnt/m", FSType: "fuse.rclone", Minor: 58}
	fuseEntry2 = mountinfo.Entry{MountPoint: "/mnt/m", FSType: "fuse.rclone", Minor: 59}
	ext4Entry  = mountinfo.Entry{MountPoint: "/mnt/m", FSType: "ext4"}
)

func testConfig() *config.Config {
	return &config.Config{
		MountPath:      "/mnt/m",
		ExpectedFSType: "fuse.rclone",
		Recovery: config.Recovery{
			DownTimeout:   config.Duration{Duration: 30 * time.Second},
			UpWaitTimeout: config.Duration{Duration: 120 * time.Second},
		},
	}
}

// fakeMounts serves a queue of mountinfo states; the last state is repeated
// once the queue is exhausted. Every call is recorded as "mounts".
type fakeMounts struct {
	seq    *[]string
	states [][]mountinfo.Entry
	idx    int
	err    error
}

func (f *fakeMounts) mounts() ([]mountinfo.Entry, error) {
	*f.seq = append(*f.seq, "mounts")
	if f.err != nil {
		return nil, f.err
	}
	st := f.states[f.idx]
	if f.idx < len(f.states)-1 {
		f.idx++
	}
	return st, nil
}

// fakeCompose records Down/Kill/Up calls and can be configured to fail.
type fakeCompose struct {
	seq      *[]string
	downErrs []error
	downCall int
	killErr  error
	upErr    error
	onDown   func()
}

func (c *fakeCompose) Down(ctx context.Context, stop time.Duration) error {
	*c.seq = append(*c.seq, fmt.Sprintf("down %ds", int(stop.Seconds())))
	if c.onDown != nil {
		c.onDown()
	}
	var err error
	if c.downCall < len(c.downErrs) {
		err = c.downErrs[c.downCall]
	}
	c.downCall++
	return err
}

func (c *fakeCompose) Kill(ctx context.Context) error {
	*c.seq = append(*c.seq, "kill")
	return c.killErr
}

func (c *fakeCompose) Up(ctx context.Context, wait time.Duration) error {
	*c.seq = append(*c.seq, fmt.Sprintf("up %ds", int(wait.Seconds())))
	return c.upErr
}

// testHarness bundles a Recoverer with its fakes and the shared sequence log.
type testHarness struct {
	seq       []string
	mounts    *fakeMounts
	compose   *fakeCompose
	abortErr  error
	detachErr error
	rec       *Recoverer
}

func newHarness(states [][]mountinfo.Entry) *testHarness {
	h := &testHarness{}
	h.mounts = &fakeMounts{seq: &h.seq, states: states}
	h.compose = &fakeCompose{seq: &h.seq}

	d := Deps{
		Mounts: h.mounts.mounts,
		Abort: func(dev uint64) error {
			h.seq = append(h.seq, fmt.Sprintf("abort %d", dev))
			return h.abortErr
		},
		Detach: func(path string) error {
			h.seq = append(h.seq, "detach")
			return h.detachErr
		},
		Compose: h.compose,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h.rec = New(testConfig(), d)
	return h
}

// indexOf returns the first index of s in seq, or -1.
func indexOf(seq []string, s string) int {
	for i, v := range seq {
		if v == s {
			return i
		}
	}
	return -1
}

func countOf(seq []string, s string) int {
	n := 0
	for _, v := range seq {
		if v == s {
			n++
		}
	}
	return n
}

func TestRecoverSequenceHealthyMount(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{fuseEntry}, {}})
	err := h.rec.Recover(context.Background(), 1, false)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if countOf(h.seq, "abort 58") != 0 {
		t.Errorf("expected no abort, seq=%v", h.seq)
	}
	di, downi, upi := indexOf(h.seq, "detach"), indexOf(h.seq, "down 30s"), indexOf(h.seq, "up 120s")
	if di < 0 || downi < 0 || upi < 0 {
		t.Fatalf("missing step in seq=%v", h.seq)
	}
	if !(di < downi && downi < upi) {
		t.Errorf("expected detach < down < up, seq=%v", h.seq)
	}
	if countOf(h.seq, "detach") != 1 {
		t.Errorf("expected exactly 1 detach (post-down mount absent), seq=%v", h.seq)
	}
}

func TestRecoverAbortFirst(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{fuseEntry}, {}})
	err := h.rec.Recover(context.Background(), 1, true)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	ai, di := indexOf(h.seq, "abort 58"), indexOf(h.seq, "detach")
	if ai < 0 || di < 0 {
		t.Fatalf("missing step in seq=%v", h.seq)
	}
	if !(ai < di) {
		t.Errorf("expected abort before first detach, seq=%v", h.seq)
	}
}

func TestRecoverPostDownStaleCleanup(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{}, {fuseEntry}, {}})
	err := h.rec.Recover(context.Background(), 1, false)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	downi := indexOf(h.seq, "down 30s")
	upi := indexOf(h.seq, "up 120s")
	ai := indexOf(h.seq, "abort 58")
	di := indexOf(h.seq, "detach")
	if downi < 0 || upi < 0 || ai < 0 || di < 0 {
		t.Fatalf("missing step in seq=%v", h.seq)
	}
	if !(downi < ai && ai < di && di < upi) {
		t.Errorf("expected down < abort < detach < up, seq=%v", h.seq)
	}
}

func TestRecoverDownFailsFallsBackToKill(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{}})
	h.compose.downErrs = []error{errors.New("boom"), nil}
	err := h.rec.Recover(context.Background(), 1, false)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	want := []string{"down 30s", "kill", "down 0s", "up 120s"}
	got := filterSeq(h.seq, want)
	if !equalSeq(got, want) {
		t.Errorf("seq subsequence = %v, want %v (full seq=%v)", got, want, h.seq)
	}
}

func TestRecoverDownAndFallbackFail(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{}})
	h.compose.downErrs = []error{errors.New("boom1"), errors.New("boom2")}
	err := h.rec.Recover(context.Background(), 1, false)
	if err == nil || !strings.Contains(err.Error(), "compose down failed") {
		t.Fatalf("err = %v, want to contain %q", err, "compose down failed")
	}
	if countOf(h.seq, "up 120s") != 0 {
		t.Errorf("expected no up call, seq=%v", h.seq)
	}
}

func TestRecoverUpFails(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{}})
	h.compose.upErr = errors.New("nope")
	err := h.rec.Recover(context.Background(), 1, false)
	if err == nil || !strings.Contains(err.Error(), "compose up failed") {
		t.Fatalf("err = %v, want to contain %q", err, "compose up failed")
	}
}

func TestCleanupRefusesForeignFSType(t *testing.T) {
	// A foreign mount stacked on top of the FUSE mount: never touched (I-2).
	h := newHarness([][]mountinfo.Entry{{fuseEntry, ext4Entry}})
	err := h.rec.CleanupMount(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want to contain %q", err, "refusing")
	}
	if countOf(h.seq, "abort 58") != 0 || countOf(h.seq, "detach") != 0 {
		t.Errorf("expected no abort/detach, seq=%v", h.seq)
	}
}

func TestCleanupForeignUnderlayOnly(t *testing.T) {
	// mount_path is itself a mountpoint (e.g. self-bind + make-rshared) and
	// no FUSE mount is left: nothing to clean, the underlay is not touched.
	h := newHarness([][]mountinfo.Entry{{ext4Entry}})
	if err := h.rec.CleanupMount(context.Background(), true); err != nil {
		t.Fatalf("CleanupMount: %v", err)
	}
	if countOf(h.seq, "detach") != 0 {
		t.Errorf("expected no detach, seq=%v", h.seq)
	}
}

func TestCleanupFuseOverForeignUnderlay(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{ext4Entry, fuseEntry}, {ext4Entry}})
	if err := h.rec.CleanupMount(context.Background(), true); err != nil {
		t.Fatalf("CleanupMount: %v", err)
	}
	want := []string{"abort 58", "detach"}
	if got := filterSeq(h.seq, want); !equalSeq(got, want) {
		t.Errorf("seq = %v, want exactly one abort+detach", h.seq)
	}
}

func TestRecoverWithForeignUnderlay(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{ext4Entry}})
	if err := h.rec.Recover(context.Background(), 1, true); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	want := []string{"down 30s", "up 120s"}
	if got := filterSeq(h.seq, want); !equalSeq(got, want) {
		t.Errorf("seq = %v, want down then up", h.seq)
	}
}

func TestCleanupAbortErrorContinues(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{fuseEntry}, {}})
	h.abortErr = errors.New("abort failed")
	if err := h.rec.CleanupMount(context.Background(), true); err != nil {
		t.Fatalf("CleanupMount: %v", err)
	}
	ai, di := indexOf(h.seq, "abort 58"), indexOf(h.seq, "detach")
	if ai < 0 || di < 0 || ai > di {
		t.Errorf("expected abort then detach, seq=%v", h.seq)
	}
}

func TestCleanupDetachErrorReturned(t *testing.T) {
	busy := errors.New("busy")
	h := newHarness([][]mountinfo.Entry{{fuseEntry}})
	h.detachErr = busy
	if err := h.rec.CleanupMount(context.Background(), false); !errors.Is(err, busy) {
		t.Fatalf("CleanupMount err = %v, want %v", err, busy)
	}

	h2 := newHarness([][]mountinfo.Entry{{fuseEntry}})
	h2.detachErr = busy
	err := h2.rec.Recover(context.Background(), 1, false)
	if err == nil || !strings.Contains(err.Error(), "pre-down cleanup") {
		t.Fatalf("Recover err = %v, want to contain %q", err, "pre-down cleanup")
	}
	if countOf(h2.seq, "down 30s") != 0 {
		t.Errorf("expected no down, seq=%v", h2.seq)
	}
}

func TestCleanupStackedMounts(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{fuseEntry, fuseEntry2}, {fuseEntry2}, {}})
	err := h.rec.CleanupMount(context.Background(), false)
	if err != nil {
		t.Fatalf("CleanupMount: %v", err)
	}
	if countOf(h.seq, "detach") != 2 {
		t.Errorf("expected 2 detach, seq=%v", h.seq)
	}
}

func TestCleanupGivesUpAfterMax(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{fuseEntry}})
	err := h.rec.CleanupMount(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("err = %v, want to contain %q", err, "still present")
	}
	if countOf(h.seq, "detach") != maxDetach {
		t.Errorf("expected %d detach, got seq=%v", maxDetach, h.seq)
	}
}

func TestCleanupNoMount(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{}})
	err := h.rec.CleanupMount(context.Background(), true)
	if err != nil {
		t.Fatalf("CleanupMount: %v", err)
	}
	if countOf(h.seq, "detach") != 0 {
		t.Errorf("expected no detach, seq=%v", h.seq)
	}
}

func TestRecoverContextCanceled(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := h.rec.Recover(ctx, 1, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if countOf(h.seq, "down 30s") != 0 || countOf(h.seq, "down 0s") != 0 ||
		countOf(h.seq, "kill") != 0 || countOf(h.seq, "detach") != 0 {
		t.Errorf("expected no down/kill/detach call, seq=%v", h.seq)
	}
}

func TestRecoverShutdownDuringDownSkipsKillFallback(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{}})
	ctx, cancel := context.WithCancel(context.Background())
	h.compose.onDown = cancel
	h.compose.downErrs = []error{errors.New("signal: killed")}
	err := h.rec.Recover(ctx, 1, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if countOf(h.seq, "kill") != 0 || countOf(h.seq, "down 0s") != 0 || countOf(h.seq, "up 120s") != 0 {
		t.Errorf("expected no kill/down 0s/up after a canceled down, seq=%v", h.seq)
	}
}

// filterSeq returns the elements of seq that are also in want, preserving
// seq's order (duplicates in want are matched positionally as they occur).
func filterSeq(seq, want []string) []string {
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}
	var out []string
	for _, s := range seq {
		if wantSet[s] {
			out = append(out, s)
		}
	}
	return out
}

func equalSeq(a, b []string) bool {
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

func TestCleanupMountHonorsCanceledContext(t *testing.T) {
	h := newHarness([][]mountinfo.Entry{{fuseEntry}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := h.rec.CleanupMount(ctx, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if countOf(h.seq, "detach") != 0 || indexOf(h.seq, "abort 58") != -1 {
		t.Fatalf("seq = %v, want no abort/detach after cancel", h.seq)
	}
}
