// Package probe checks whether a mount point is responsive.
//
// Local performs the actual filesystem check and is meant to run inside a
// short-lived subprocess (see the "probe" subcommand of cmd/mount-manager),
// since a hung FUSE mount can block the calling goroutine indefinitely. The
// main process never touches the mount filesystem directly; it only runs
// Local out-of-process via Runner.Probe.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Result is the outcome of a mount point probe.
type Result struct {
	OK         bool   `json:"ok"`
	HasEntries bool   `json:"has_entries"`
	Timeout    bool   `json:"timeout,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Local checks path directly in the current process. It stats path, lists
// one directory entry, and, if canary is non-empty, stats path/canary.
func Local(path, canary string) Result {
	info, err := os.Stat(path)
	if err != nil {
		return Result{Error: err.Error()}
	}
	if !info.IsDir() {
		return Result{Error: "not a directory"}
	}

	f, err := os.Open(path)
	if err != nil {
		return Result{Error: err.Error()}
	}
	defer f.Close()

	hasEntries := false
	names, err := f.Readdirnames(1)
	if err != nil && err != io.EOF {
		return Result{Error: err.Error()}
	}
	if len(names) > 0 {
		hasEntries = true
	}

	if canary != "" {
		if _, err := os.Stat(filepath.Join(path, canary)); err != nil {
			return Result{Error: "canary: " + err.Error()}
		}
	}

	return Result{OK: true, HasEntries: hasEntries}
}

// maxStuck is the number of abandoned probe processes (killed but never
// reaped within the post-kill grace window) allowed to accumulate before
// Runner.Probe refuses to start new ones.
const maxStuck = 3

// Runner executes Local out-of-process by re-invoking the probe subcommand.
type Runner struct {
	Exe     string        // executable to run, e.g. os.Executable() in production
	Prefix  []string      // arguments inserted before "probe" (tests use the go test helper-process trick)
	Env     []string      // appended to os.Environ() for the subprocess
	Timeout time.Duration // how long to wait before killing the subprocess

	stuck atomic.Int32
}

// Probe runs Local in a subprocess and waits for its JSON result, honoring
// r.Timeout and ctx. If the subprocess does not exit promptly after being
// killed, it is abandoned in the background and counted against stuck, up to
// maxStuck concurrent abandoned processes.
func (r *Runner) Probe(ctx context.Context, path, canary string) Result {
	if r.stuck.Load() >= maxStuck {
		return Result{Error: "probe_stuck: 3 abandoned probe processes"}
	}

	args := append(append([]string{}, r.Prefix...), "probe", "--path", path)
	if canary != "" {
		args = append(args, "--canary", canary)
	}

	cmd := exec.Command(r.Exe, args...)
	cmd.Env = append(os.Environ(), r.Env...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return Result{Error: fmt.Sprintf("probe start: %s", err)}
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	timer := time.NewTimer(r.Timeout)
	defer timer.Stop()

	select {
	case waitErr := <-done:
		var res Result
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
			if waitErr != nil {
				return Result{Error: fmt.Sprintf("probe output invalid: %s: %s", err, waitErr)}
			}
			return Result{Error: fmt.Sprintf("probe output invalid: %s", err)}
		}
		return res

	case <-timer.C:
		r.killAndReap(cmd, done)
		return Result{Timeout: true, Error: fmt.Sprintf("probe timeout after %s", r.Timeout)}

	case <-ctx.Done():
		r.killAndReap(cmd, done)
		return Result{Timeout: true, Error: "probe canceled"}
	}
}

// killAndReap kills the subprocess and waits up to 2 seconds for it to be
// reaped. If it is not reaped in time, it is abandoned: stuck is
// incremented, and a background goroutine waits for it and decrements stuck
// once it eventually exits.
func (r *Runner) killAndReap(cmd *exec.Cmd, done chan error) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		r.stuck.Add(1)
		go func() {
			<-done
			r.stuck.Add(-1)
		}()
	}
}
