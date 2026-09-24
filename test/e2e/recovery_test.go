//go:build e2e

package e2e

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestE2E_KillRcloneRecovers (E3): killing the rclone-mount container must
// be detected and fully recovered: a new superdufs container comes up
// (I-2/I-7 sequence), the host mount is healthy again, and superdufs sees
// the canary file again.
func TestE2E_KillRcloneRecovers(t *testing.T) {
	requireRoot(t)
	e := newEnv(t)
	writeManagerConfig(e, nil)

	compose(e, 3*time.Minute, "up", "-d", "--pull", "never", "--wait", "--wait-timeout", "120")
	startManager(e)
	e.hook.waitEvent("stack_up", 0, 60*time.Second)

	id0 := containerID(e, "superdufs")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	out, err := exec.CommandContext(ctx, "docker", "kill", "rclone-mount").CombinedOutput()
	cancel()
	if err != nil {
		t.Fatalf("docker kill rclone-mount: %v\n%s", err, out)
	}

	e.hook.waitEvent("failure_detected", 0, 30*time.Second)
	e.hook.waitEvent("recovery_started", 0, 10*time.Second)
	e.hook.waitEvent("recovery_succeeded", 0, 180*time.Second)

	if !hostMountHealthy(e) {
		t.Fatal("hostMountHealthy: mount not healthy after recovery")
	}

	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	out, err = exec.CommandContext(ctx, "docker", "compose", "-f", e.composeFile, "--project-directory", e.work,
		"exec", "-T", "superdufs", "ls", "/data/canary.txt").CombinedOutput()
	cancel()
	if err != nil {
		t.Fatalf("superdufs does not see the mount after recovery: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "canary.txt") {
		t.Fatalf("superdufs exec ls /data/canary.txt output = %q, want to contain canary.txt", out)
	}

	if id1 := containerID(e, "superdufs"); id1 == id0 {
		t.Fatalf("containerID(superdufs) unchanged after recovery: %s", id1)
	}

	httpCtx, httpCancel := context.WithTimeout(context.Background(), 5*time.Second)
	req, err := http.NewRequestWithContext(httpCtx, http.MethodGet, "http://127.0.0.1:5000/", nil)
	if err != nil {
		httpCancel()
		t.Fatalf("build GET request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	httpCancel()
	if err != nil {
		t.Fatalf("GET http://127.0.0.1:5000/: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET http://127.0.0.1:5000/ status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestE2E_PausedRcloneRecovers (E4): pausing the rclone-mount container
// wedges the FUSE mount; the probe must time out (not hang the check loop)
// and recovery must still converge (I-3, I-7).
func TestE2E_PausedRcloneRecovers(t *testing.T) {
	requireRoot(t)
	e := newEnv(t)
	writeManagerConfig(e, nil)

	compose(e, 3*time.Minute, "up", "-d", "--pull", "never", "--wait", "--wait-timeout", "120")
	startManager(e)
	e.hook.waitEvent("stack_up", 0, 60*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	out, err := exec.CommandContext(ctx, "docker", "pause", "rclone-mount").CombinedOutput()
	cancel()
	if err != nil {
		t.Fatalf("docker pause rclone-mount: %v\n%s", err, out)
	}

	ev, _ := e.hook.waitEvent("failure_detected", 0, 40*time.Second)
	if !strings.Contains(ev.Detail, "probe timeout") {
		t.Fatalf("failure_detected Detail = %q, want to contain %q", ev.Detail, "probe timeout")
	}

	e.hook.waitEvent("recovery_started", 0, 10*time.Second)
	e.hook.waitEvent("recovery_succeeded", 0, 240*time.Second)

	if !hostMountHealthy(e) {
		t.Fatal("hostMountHealthy: mount not healthy after recovery")
	}

	if state := containerState(e, "rclone-mount"); state != "running" {
		t.Fatalf("containerState(rclone-mount) = %q, want %q", state, "running")
	}

	if code := runCheck(e); code != 0 {
		t.Fatalf("runCheck = %d, want 0", code)
	}
}

// TestE2E_StaleMountCleanedWhenIdle (E5): a stale host mount left behind
// after the stack is torn down (without the manager running) must be
// cleaned up on manager startup, without starting the compose stack (D14,
// I-1).
func TestE2E_StaleMountCleanedWhenIdle(t *testing.T) {
	requireRoot(t)
	e := newEnv(t)
	writeManagerConfig(e, nil)

	compose(e, 3*time.Minute, "up", "-d", "--pull", "never", "--wait", "--wait-timeout", "120")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	out, err := exec.CommandContext(ctx, "docker", "kill", "rclone-mount").CombinedOutput()
	cancel()
	if err != nil {
		t.Fatalf("docker kill rclone-mount: %v\n%s", err, out)
	}

	// The kernel may answer stat on the mount root from its attribute cache
	// for a short while after the FUSE daemon dies, so poll until the stale
	// connection is visible.
	if err := waitStale(e.mountPath, 10*time.Second); !errors.Is(err, syscall.ENOTCONN) {
		t.Fatalf("precondition: expected stale mount (R10): got %v, want ENOTCONN", err)
	}

	compose(e, 5*time.Second+10*time.Second, "down", "-t", "5")

	if !mountPresent(e) {
		t.Fatal("precondition: expected stale mount still present after compose down")
	}

	startManager(e)

	e.hook.waitEvent("stale_mount_cleaned", 0, 20*time.Second)

	if mountPresent(e) {
		t.Fatal("mountPresent: stale mount still present after stale_mount_cleaned")
	}

	time.Sleep(5 * time.Second)
	e.hook.assertNoEvent("recovery_started", 0)

	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	out, err = exec.CommandContext(ctx, "docker", "compose", "-f", e.composeFile, "--project-directory", e.work,
		"ps", "-a", "-q").CombinedOutput()
	cancel()
	if err != nil {
		t.Fatalf("docker compose ps -a -q: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("docker compose ps -a -q = %q, want empty (no stack started, D14)", out)
	}
}

// TestE2E_PartialStackAfterBootRecovers (E6): a partial stack state as it
// could occur after a host boot (superdufs running via restart: always,
// rclone-mount stopped) must be detected once the manager starts and
// recovered after the startup grace (D5).
func TestE2E_PartialStackAfterBootRecovers(t *testing.T) {
	requireRoot(t)
	e := newEnv(t)
	writeManagerConfig(e, nil)

	compose(e, 3*time.Minute, "up", "-d", "--pull", "never", "--wait", "--wait-timeout", "120")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	out, err := exec.CommandContext(ctx, "docker", "compose", "-f", e.composeFile, "--project-directory", e.work,
		"stop", "rclone-mount").CombinedOutput()
	cancel()
	if err != nil {
		t.Fatalf("docker compose stop rclone-mount: %v\n%s", err, out)
	}

	if state := containerState(e, "superdufs"); state != "running" {
		t.Fatalf("precondition: containerState(superdufs) = %q, want %q", state, "running")
	}

	startManager(e)

	e.hook.waitEvent("waiting", 0, 10*time.Second)
	e.hook.waitEvent("failure_detected", 0, 30*time.Second)
	e.hook.waitEvent("recovery_succeeded", 0, 180*time.Second)

	if !hostMountHealthy(e) {
		t.Fatal("hostMountHealthy: mount not healthy after recovery")
	}

	if code := runCheck(e); code != 0 {
		t.Fatalf("runCheck = %d, want 0", code)
	}
}
