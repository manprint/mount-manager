//go:build e2e

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestE2E_WaitThenMonitor (E1): the manager waits idle while the compose
// stack is down, then transitions to monitoring once the stack is brought
// up manually, with the host mount and superdufs both seeing the rclone
// mount (D11 bootstrap) and `check` reporting OK.
func TestE2E_WaitThenMonitor(t *testing.T) {
	e := newEnv(t)
	writeManagerConfig(e, nil)

	startManager(e)
	e.hook.waitEvent("startup", 0, 10*time.Second)
	e.hook.waitEvent("waiting", 0, 10*time.Second)

	time.Sleep(3 * time.Second)
	e.hook.assertNoEvent("stack_up", 0)

	if code := runCheck(e); code != 2 {
		t.Fatalf("runCheck (idle) = %d, want 2", code)
	}

	compose(e, 3*time.Minute, "up", "-d", "--pull", "never", "--wait", "--wait-timeout", "120")

	e.hook.waitEvent("stack_up", 0, 60*time.Second)

	if !hostMountHealthy(e) {
		t.Fatal("hostMountHealthy: mount not healthy after stack_up")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "compose", "-f", e.composeFile, "--project-directory", e.work,
		"exec", "-T", "superdufs", "ls", "/data/canary.txt").CombinedOutput()
	if err != nil {
		t.Fatalf("superdufs does not see the mount (D11 bootstrap): %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "canary.txt") {
		t.Fatalf("superdufs exec ls /data/canary.txt output = %q, want to contain canary.txt", out)
	}

	if code := runCheck(e); code != 0 {
		t.Fatalf("runCheck (ok) = %d, want 0", code)
	}

	e.hook.assertNoEvent("recovery_started", 0)
}

// TestE2E_ManualDownNoRecovery (E2): stopping the compose stack manually
// while the manager is monitoring must be observed as a clean transition
// to waiting (stack_stopped), never as a failure requiring recovery (I-1).
func TestE2E_ManualDownNoRecovery(t *testing.T) {
	e := newEnv(t)
	writeManagerConfig(e, nil)

	compose(e, 3*time.Minute, "up", "-d", "--pull", "never", "--wait", "--wait-timeout", "120")

	startManager(e)
	e.hook.waitEvent("stack_up", 0, 60*time.Second)

	compose(e, 30*time.Second, "down", "-t", "10")

	e.hook.waitEvent("stack_stopped", 0, 30*time.Second)

	time.Sleep(10 * time.Second)
	e.hook.assertNoEvent("recovery_started", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "compose", "-f", e.composeFile, "--project-directory", e.work,
		"ps", "-a", "-q").CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose ps -a -q: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("docker compose ps -a -q = %q, want empty (no leftover containers)", out)
	}
}
