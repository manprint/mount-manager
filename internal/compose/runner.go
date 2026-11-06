package compose

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// Runner executes an external command and returns its stdout/stderr.
type Runner interface {
	Run(ctx context.Context, timeout time.Duration, name string, args ...string) (stdout, stderr []byte, err error)
}

// ExecRunner runs commands via os/exec, terminating them with SIGTERM on
// timeout and enforcing a bounded wait for the process to exit afterwards.
// Each command runs in its own process group, killed once the command was
// timed out or canceled, so that no child (e.g. the compose plugin spawned
// by the docker CLI) outlives it and overlaps the next compose command.
type ExecRunner struct{}

// Run executes name with args under a timeout, returning captured
// stdout/stderr. On timeout the process is signalled with SIGTERM and the
// returned error reports "timeout after <timeout>", unless the command
// succeeded anyway.
func (ExecRunner) Run(ctx context.Context, timeout time.Duration, name string, args ...string) (stdout, stderr []byte, err error) {
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx2, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	if ctx2.Err() != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	stdout = outBuf.Bytes()
	stderr = errBuf.Bytes()

	if runErr != nil && ctx2.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timeout after %s", timeout)
		return stdout, stderr, err
	}

	err = runErr
	return stdout, stderr, err
}
