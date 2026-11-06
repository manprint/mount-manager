//go:build e2e

// Package e2e contains real, non-mocked end-to-end scenarios for
// mount-manager: they build/require the real binary, run the real compose
// stack (rclone-mount + superdufs) against a generated local alias remote,
// and exercise `mount-manager run`/`check` for real. See I-6: only a
// generated local alias remote is ever used here; the repository's own
// ./config directory (real rclone remotes) is never read or mounted.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"mount-manager/internal/mountinfo"
)

// repoRoot returns the absolute, cleaned path to the repository root,
// derived from this file's own location.
func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	return filepath.Clean(filepath.Join(dir, "..", ".."))
}

// managerBin returns the path to the mount-manager binary under test: the
// MM_BIN environment variable if set, otherwise <repo>/bin/mount-manager.
// It fails the test immediately if the binary is not executable.
func managerBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("MM_BIN")
	if bin == "" {
		bin = filepath.Join(repoRoot(), "bin", "mount-manager")
	}
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		t.Fatalf("build first: go build -o bin/mount-manager ./cmd/mount-manager")
	}
	return bin
}

// env holds the resources for one e2e scenario run.
type env struct {
	t           *testing.T
	work        string
	composeFile string
	mountPath   string
	cfgPath     string
	hook        *hookServer
	mgr         *exec.Cmd
	mgrLog      *os.File
}

// sanitizeName makes t.Name() safe for use as a directory component: "/"
// (from subtests) is replaced with "_".
func sanitizeName(name string) string {
	return strings.ReplaceAll(name, "/", "_")
}

// newEnv builds a fresh e2e environment: it runs preflight checks, creates
// an isolated work directory with its own copy of compose.yml, a generated
// local alias remote (I-6) and mount fixtures, and registers teardown as
// the first cleanup so it always runs even if a later step in newEnv fails.
func newEnv(t *testing.T) *env {
	t.Helper()

	preflight(t)

	work := filepath.Join(repoRoot(), "test", "e2e", ".work",
		fmt.Sprintf("%s-%d", sanitizeName(t.Name()), time.Now().UnixNano()))
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work dir: %v", err)
	}

	e := &env{
		t:           t,
		work:        work,
		composeFile: filepath.Join(work, "compose.yml"),
		mountPath:   filepath.Join(work, "mymount"),
		cfgPath:     filepath.Join(work, "manager.yml"),
	}
	t.Cleanup(e.teardown)

	copyFile(t, filepath.Join(repoRoot(), "compose.yml"), e.composeFile)

	configDir := filepath.Join(work, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	rcloneConf := "[remote]\ntype = alias\nremote = /config/e2e-remote\n"
	if err := os.WriteFile(filepath.Join(configDir, "rclone.conf"), []byte(rcloneConf), 0o644); err != nil {
		t.Fatalf("write rclone.conf: %v", err)
	}
	remoteDir := filepath.Join(configDir, "e2e-remote")
	if err := os.MkdirAll(filepath.Join(remoteDir, "dir1"), 0o755); err != nil {
		t.Fatalf("mkdir e2e-remote/dir1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "canary.txt"), []byte("canary\n"), 0o644); err != nil {
		t.Fatalf("write canary.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "dir1", "file.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write dir1/file.txt: %v", err)
	}
	if err := os.MkdirAll(e.mountPath, 0o755); err != nil {
		t.Fatalf("mkdir mymount: %v", err)
	}

	if os.Geteuid() == 0 {
		chownAll(t, work)
	}

	assertRemoteIsLocal(t, work)

	e.hook = newHookServer(t)

	return e
}

// preflight fails fast if the production stack looks like it is running,
// or the required port/images are not ready.
func preflight(t *testing.T) {
	t.Helper()

	out, err := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}").CombinedOutput()
	if err != nil {
		t.Fatalf("docker ps -a: %v\n%s", err, out)
	}
	names := strings.Split(string(out), "\n")
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "rclone-mount" || n == "superdufs" {
			t.Fatalf("stop the production stack first: container %q exists", n)
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:5000")
	if err != nil {
		t.Fatalf("port 127.0.0.1:5000 not free: %v", err)
	}
	ln.Close()

	images := []string{"rclone/rclone:latest", "fabiop85/superdufs:v1.5.0"}
	// Pull missing images directly so the repository project directory is
	// never used as a compose project (I-6).
	for _, img := range images {
		if err := exec.Command("docker", "image", "inspect", img).Run(); err == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		out, err := exec.CommandContext(ctx, "docker", "pull", img).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("docker pull %s: %v\n%s", img, err, out)
		}
	}
}

// copyFile copies src to dst byte-for-byte.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// chownAll makes work owned by uid/gid 1000 (the compose stack's user),
// used when the test process runs as root.
func chownAll(t *testing.T, work string) {
	t.Helper()
	err := filepath.WalkDir(work, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, 1000, 1000)
	})
	if err != nil {
		t.Fatalf("chown -R 1000:1000 %s: %v", work, err)
	}
}

// assertRemoteIsLocal verifies (I-6) that the generated rclone.conf in work
// is exactly the expected local alias remote, and that its config
// directory lives under work and is not the repository's own ./config.
func assertRemoteIsLocal(t *testing.T, work string) {
	t.Helper()
	want := "[remote]\ntype = alias\nremote = /config/e2e-remote\n"
	confPath := filepath.Join(work, "config", "rclone.conf")
	got, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read rclone.conf: %v", err)
	}
	if string(got) != want {
		t.Fatalf("rclone.conf = %q, want %q", got, want)
	}
	configDir := filepath.Join(work, "config")
	repoConfigDir := filepath.Join(repoRoot(), "config")
	rel, err := filepath.Rel(work, configDir)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("config dir %s is not under work %s", configDir, work)
	}
	if configDir == repoConfigDir {
		t.Fatalf("config dir must not be the repository's ./config (I-6)")
	}
}

// writeManagerConfig marshals a manager configuration (with the fixed
// e2e-test values, mutable via overrides) to e.cfgPath.
func writeManagerConfig(e *env, overrides func(*map[string]any)) {
	e.t.Helper()

	m := map[string]any{
		"mount_path":        e.mountPath,
		"compose_file":      e.composeFile,
		"check_interval":    "1s",
		"check_timeout":     "3s",
		"failure_threshold": 2,
		// Above the time rclone-mount needs to turn healthy under load: a
		// shorter grace starts a recovery that races the test's own `up --wait`.
		"startup_grace": "15s",
		"log_level":     "debug",
		"recovery": map[string]any{
			"down_timeout":    "10s",
			"up_wait_timeout": "90s",
			"verify_timeout":  "60s",
			"command_grace":   "30s",
			"backoff_initial": "2s",
			"backoff_max":     "8s",
		},
		"checks": map[string]any{
			"canary": "canary.txt",
			"http": map[string]any{
				"url":           "http://127.0.0.1:5000/",
				"timeout":       "3s",
				"accept_status": []int{200, 401, 403},
			},
			"exec": map[string]any{
				"service": "superdufs",
				"path":    "/data",
				"timeout": "10s",
			},
		},
		"webhook": map[string]any{
			"url":     e.hook.URL,
			"timeout": "2s",
		},
	}
	if overrides != nil {
		overrides(&m)
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		e.t.Fatalf("marshal manager config: %v", err)
	}
	if err := os.WriteFile(e.cfgPath, data, 0o644); err != nil {
		e.t.Fatalf("write manager config: %v", err)
	}
}

// hookEvent mirrors notify.Event's JSON shape, decoded independently so
// the harness does not import the (non-test) notify package's internals
// beyond its wire format.
type hookEvent struct {
	Event     string    `json:"event"`
	Timestamp time.Time `json:"timestamp"`
	Host      string    `json:"host"`
	MountPath string    `json:"mount_path"`
	Detail    string    `json:"detail"`
	Attempt   int       `json:"attempt"`
}

// hookServer is an httptest.Server that records the JSON events posted to
// it by mount-manager's webhook notifier.
type hookServer struct {
	t      *testing.T
	srv    *httptest.Server
	URL    string
	mu     sync.Mutex
	events []hookEvent
}

// newHookServer starts a hookServer; it is closed by env.teardown.
func newHookServer(t *testing.T) *hookServer {
	t.Helper()
	h := &hookServer{t: t}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev hookEvent
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			h.mu.Lock()
			h.events = append(h.events, ev)
			h.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	h.URL = h.srv.URL
	return h
}

// close shuts down the underlying httptest.Server.
func (h *hookServer) close() {
	h.srv.Close()
}

// waitEvent polls (every 200ms) for an event named name at index >= after,
// failing the test with the list of events received so far if timeout
// elapses first.
func (h *hookServer) waitEvent(name string, after int, timeout time.Duration) (hookEvent, int) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		h.mu.Lock()
		for i := after; i < len(h.events); i++ {
			if h.events[i].Event == name {
				ev := h.events[i]
				h.mu.Unlock()
				return ev, i
			}
		}
		snapshot := append([]hookEvent(nil), h.events...)
		h.mu.Unlock()

		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for event %q (after index %d); events received: %+v", name, after, snapshot)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// assertNoEvent fails the test if any event named name is present at index
// >= after.
func (h *hookServer) assertNoEvent(name string, after int) {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := after; i < len(h.events); i++ {
		if h.events[i].Event == name {
			h.t.Fatalf("unexpected event %q at index %d: %+v", name, i, h.events[i])
		}
	}
}

// startManager starts `mount-manager run --config e.cfgPath`, redirecting
// its stdout+stderr to <work>/manager.log.
func startManager(e *env) {
	e.t.Helper()
	bin := managerBin(e.t)

	logFile, err := os.Create(filepath.Join(e.work, "manager.log"))
	if err != nil {
		e.t.Fatalf("create manager.log: %v", err)
	}
	e.mgrLog = logFile

	cmd := exec.Command(bin, "run", "--config", e.cfgPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("start manager: %v", err)
	}
	e.mgr = cmd
}

// stopManager sends SIGTERM to the manager process and waits up to 20s for
// it to exit, then force-kills it.
func stopManager(e *env) {
	e.t.Helper()
	if e.mgr == nil || e.mgr.Process == nil {
		return
	}

	done := make(chan error, 1)
	go func() { done <- e.mgr.Wait() }()

	_ = e.mgr.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = e.mgr.Process.Kill()
		<-done
	}
	e.mgr = nil
}

// compose runs `docker compose -f <work>/compose.yml --project-directory
// <work> <args...>` with the given timeout, failing the test with the
// command output on error.
func compose(e *env, timeout time.Duration, args ...string) []byte {
	e.t.Helper()
	fullArgs := append([]string{"compose", "-f", e.composeFile, "--project-directory", e.work}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", fullArgs...).CombinedOutput()
	if err != nil {
		e.t.Fatalf("docker %s: %v\n%s", strings.Join(fullArgs, " "), err, out)
	}
	return out
}

// hostMountHealthy reports whether the host's own mountinfo shows a
// fuse.rclone mount at e.mountPath and reading <mountPath>/canary.txt
// succeeds and returns "canary\n". Both checks run with a 5s timeout so a
// hung mount cannot hang the test (I-3 also applies to the test harness).
func hostMountHealthy(e *env) bool {
	type result struct {
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		entries, err := mountinfo.Read()
		if err != nil {
			done <- result{false}
			return
		}
		top, ok := mountinfo.Top(entries, e.mountPath)
		if !ok || top.FSType != "fuse.rclone" {
			done <- result{false}
			return
		}
		data, err := os.ReadFile(filepath.Join(e.mountPath, "canary.txt"))
		if err != nil || string(data) != "canary\n" {
			done <- result{false}
			return
		}
		done <- result{true}
	}()

	select {
	case r := <-done:
		return r.ok
	case <-time.After(5 * time.Second):
		return false
	}
}

// runCheck runs `mount-manager check --config e.cfgPath` and returns its
// exit code.
func runCheck(e *env) int {
	e.t.Helper()
	bin := managerBin(e.t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "check", "--config", e.cfgPath)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	e.t.Fatalf("run check: %v", err)
	return -1
}

// teardown stops the manager, tears down the compose stack, force-unmounts
// any leftover mount, prints the manager log on failure and removes the
// work directory. It is safe to call multiple times.
func (e *env) teardown() {
	stopManager(e)

	if e.composeFile != "" {
		if _, err := os.Stat(e.composeFile); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			out, err := exec.CommandContext(ctx, "docker", "compose", "-f", e.composeFile,
				"--project-directory", e.work, "down", "-t", "5").CombinedOutput()
			cancel()
			if err != nil {
				e.t.Logf("teardown: docker compose down: %v\n%s", err, out)
			}
		}
	}

	if entries, err := mountinfo.Read(); err == nil {
		if _, ok := mountinfo.Top(entries, e.mountPath); ok {
			if os.Geteuid() == 0 {
				for i := 0; i < 5; i++ {
					err := syscall.Unmount(e.mountPath, syscall.MNT_DETACH)
					if err != nil {
						e.t.Logf("teardown: unmount %s: %v", e.mountPath, err)
					}
					entries, err := mountinfo.Read()
					if err != nil {
						break
					}
					if _, ok := mountinfo.Top(entries, e.mountPath); !ok {
						break
					}
				}
			} else {
				out, err := exec.Command("fusermount3", "-u", "-z", e.mountPath).CombinedOutput()
				if err != nil {
					e.t.Logf("teardown: fusermount3 -u -z %s: %v\n%s", e.mountPath, err, out)
				}
			}
		}
	}

	if e.hook != nil {
		e.hook.close()
	}

	if e.t.Failed() {
		printLastLines(e.t, filepath.Join(e.work, "manager.log"), 100)
	}

	if e.mgrLog != nil {
		_ = e.mgrLog.Close()
	}

	// Never walk into a mount that is still there: RemoveAll could hang on
	// a dead FUSE mount or delete files through a live one.
	if mountPresent(e) {
		e.t.Errorf("teardown: %s is still mounted, leaving work dir %s in place", e.mountPath, e.work)
		return
	}
	if err := os.RemoveAll(e.work); err != nil {
		e.t.Logf("teardown: remove work dir %s: %v", e.work, err)
	}
}

// requireRoot fails the test (via t.Fatal, not t.Skip: a silently skipped
// root scenario would report green without ever running) unless the test
// process is running as root.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("root required: run via e2e-root.sh")
	}
}

// containerID returns the full container ID of the named container, failing
// the test on error.
func containerID(e *env, name string) string {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.Id}}", name).CombinedOutput()
	if err != nil {
		e.t.Fatalf("docker inspect -f {{.Id}} %s: %v\n%s", name, err, out)
	}
	return strings.TrimSpace(string(out))
}

// containerState returns the named container's State.Status (e.g.
// "running", "paused", "exited"), failing the test on error.
func containerState(e *env, name string) string {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Status}}", name).CombinedOutput()
	if err != nil {
		e.t.Fatalf("docker inspect -f {{.State.Status}} %s: %v\n%s", name, err, out)
	}
	return strings.TrimSpace(string(out))
}

// statErr runs os.Stat(path) in a goroutine and returns its error, or an
// error "stat timeout" if it does not complete within 5s (a stale FUSE
// mount can hang stat indefinitely).
func statErr(path string) error {
	done := make(chan error, 1)
	go func() {
		_, err := os.Stat(path)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("stat timeout")
	}
}

// waitStale polls path with statErr and a bounded directory read every
// 500ms until one of them fails with ENOTCONN or timeout elapses; it returns
// the last error observed (nil if the path stayed readable).
func waitStale(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := statErr(path)
		if err == nil {
			err = readDirErr(path)
		}
		if errors.Is(err, syscall.ENOTCONN) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// readDirErr runs os.ReadDir(path) in a goroutine and returns its error, or
// "readdir timeout" after 5s.
func readDirErr(path string) error {
	done := make(chan error, 1)
	go func() {
		_, err := os.ReadDir(path)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("readdir timeout")
	}
}

// mountPresent reports whether the host's own mountinfo shows any mount
// stacked at e.mountPath.
func mountPresent(e *env) bool {
	entries, err := mountinfo.Read()
	if err != nil {
		return false
	}
	_, ok := mountinfo.Top(entries, e.mountPath)
	return ok
}

// printLastLines prints up to the last n lines of path via t.Logf.
func printLastLines(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if len(lines) == 0 {
		return
	}
	t.Logf("last %d lines of %s:\n%s", len(lines), path, strings.Join(lines, "\n"))
}
