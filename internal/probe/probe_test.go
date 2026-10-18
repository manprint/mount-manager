package probe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalEmptyDir(t *testing.T) {
	dir := t.TempDir()
	res := Local(dir, "")
	if !res.OK {
		t.Fatalf("OK = false, err = %q", res.Error)
	}
	if res.HasEntries {
		t.Fatalf("HasEntries = true, want false")
	}
}

func TestLocalWithEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Local(dir, "")
	if !res.OK {
		t.Fatalf("OK = false, err = %q", res.Error)
	}
	if !res.HasEntries {
		t.Fatalf("HasEntries = false, want true")
	}
}

func TestLocalCanaryPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "canary.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Local(dir, "canary.txt")
	if !res.OK {
		t.Fatalf("OK = false, err = %q", res.Error)
	}
}

func TestLocalCanaryMissing(t *testing.T) {
	dir := t.TempDir()
	res := Local(dir, "canary.txt")
	if res.OK {
		t.Fatalf("OK = true, want false")
	}
	if !strings.Contains(res.Error, "canary") {
		t.Fatalf("Error = %q, want to contain %q", res.Error, "canary")
	}
}

func TestLocalNotDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Local(file, "")
	if res.OK {
		t.Fatalf("OK = true, want false")
	}
	if !strings.Contains(res.Error, "not a directory") {
		t.Fatalf("Error = %q, want to contain %q", res.Error, "not a directory")
	}
}

func TestLocalMissingPath(t *testing.T) {
	res := Local(filepath.Join(t.TempDir(), "nope"), "")
	if res.OK {
		t.Fatalf("OK = true, want false")
	}
	if res.Error == "" {
		t.Fatalf("Error = %q, want non-empty", res.Error)
	}
}

// TestHelperProcess is not a real test; it is a subprocess helper invoked by
// the Runner tests below via re-exec of the test binary. See
// https://pkg.go.dev/os/exec#Command for the pattern.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("MM_PROBE_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "ok":
		fmt.Print(`{"ok":true,"has_entries":true}`)
		os.Exit(0)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "garbage":
		fmt.Print("xx")
		os.Exit(0)
	}
}

func testRunner(mode string) *Runner {
	return &Runner{
		Exe:     os.Args[0],
		Prefix:  []string{"-test.run=TestHelperProcess", "--"},
		Env:     []string{"MM_PROBE_HELPER=" + mode},
		Timeout: 5 * time.Second,
	}
}

func TestRunnerOK(t *testing.T) {
	r := testRunner("ok")
	res := r.Probe(context.Background(), "/some/path", "")
	if !res.OK || !res.HasEntries {
		t.Fatalf("res = %+v, want OK && HasEntries", res)
	}
}

func TestRunnerTimeout(t *testing.T) {
	r := testRunner("sleep")
	r.Timeout = 300 * time.Millisecond
	start := time.Now()
	res := r.Probe(context.Background(), "/some/path", "")
	elapsed := time.Since(start)
	if !res.Timeout {
		t.Fatalf("Timeout = false, want true, res = %+v", res)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %s, want < 5s", elapsed)
	}
}

func TestRunnerGarbageOutput(t *testing.T) {
	r := testRunner("garbage")
	res := r.Probe(context.Background(), "/some/path", "")
	if !strings.Contains(res.Error, "probe output invalid") {
		t.Fatalf("Error = %q, want to contain %q", res.Error, "probe output invalid")
	}
}

func TestRunnerStuckGuard(t *testing.T) {
	r := &Runner{Exe: "/nonexistent", Timeout: time.Second}
	r.stuck.Store(3)
	res := r.Probe(context.Background(), "/some/path", "")
	if !strings.Contains(res.Error, "probe_stuck") {
		t.Fatalf("Error = %q, want to contain %q", res.Error, "probe_stuck")
	}
}

func TestRunnerContextCanceled(t *testing.T) {
	r := testRunner("sleep")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := r.Probe(ctx, "/some/path", "")
	if !res.Timeout {
		t.Fatalf("Timeout = false, want true, res = %+v", res)
	}
	if !strings.Contains(res.Error, "probe canceled") {
		t.Fatalf("Error = %q, want to contain %q", res.Error, "probe canceled")
	}
}

func TestKillAndReapAbandonsStuckProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// done is never written within the 2s reap window, as for a process
	// stuck in uninterruptible sleep on a hung FUSE mount.
	done := make(chan error)
	r := &Runner{}
	start := time.Now()
	r.killAndReap(cmd, done)
	if elapsed := time.Since(start); elapsed < 2*time.Second || elapsed > 4*time.Second {
		t.Fatalf("killAndReap took %v, want ~2s", elapsed)
	}
	if got := r.stuck.Load(); got != 1 {
		t.Fatalf("stuck = %d, want 1", got)
	}

	done <- nil // the process is eventually reaped
	deadline := time.Now().Add(time.Second)
	for r.stuck.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("stuck = %d after reap, want 0", r.stuck.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
