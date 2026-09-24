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
type ExecRunner struct{}

// Run executes name with args under a timeout, returning captured
// stdout/stderr. On timeout the process is signalled with SIGTERM and the
// returned error reports "timeout after <timeout>".
func (ExecRunner) Run(ctx context.Context, timeout time.Duration, name string, args ...string) (stdout, stderr []byte, err error) {
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx2, name, args...)
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()

	stdout = outBuf.Bytes()
	stderr = errBuf.Bytes()

	if ctx2.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timeout after %s", timeout)
		return stdout, stderr, err
	}

	err = runErr
	return stdout, stderr, err
}
