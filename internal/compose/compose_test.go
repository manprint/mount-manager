package compose

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeRunner records the last Run call and returns configured output.
type fakeRunner struct {
	timeout time.Duration
	name    string
	args    []string

	stdout []byte
	stderr []byte
	err    error
}

func (f *fakeRunner) Run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, []byte, error) {
	f.timeout = timeout
	f.name = name
	f.args = args
	return f.stdout, f.stderr, f.err
}

func newTestClient(r Runner) *Client {
	return &Client{
		Runner:     r,
		Docker:     "docker",
		File:       "/proj/compose.yml",
		ProjectDir: "/proj",
		Grace:      60 * time.Second,
	}
}

func TestParsePSNDJSON(t *testing.T) {
	in := `{"Name":"a","Service":"rclone-mount","State":"running","Health":"healthy","ExitCode":0}
{"Name":"b","Service":"superdufs","State":"running","Health":"","ExitCode":0}
`
	got, err := ParsePS([]byte(in))
	if err != nil {
		t.Fatalf("ParsePS: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	want0 := Container{Name: "a", Service: "rclone-mount", State: "running", Health: "healthy", ExitCode: 0}
	want1 := Container{Name: "b", Service: "superdufs", State: "running", Health: "", ExitCode: 0}
	if got[0] != want0 {
		t.Errorf("got[0] = %+v, want %+v", got[0], want0)
	}
	if got[1] != want1 {
		t.Errorf("got[1] = %+v, want %+v", got[1], want1)
	}
}

func TestParsePSArray(t *testing.T) {
	in := `[{"Name":"a","Service":"rclone-mount","State":"running","Health":"healthy","ExitCode":0},{"Name":"b","Service":"superdufs","State":"exited","Health":"","ExitCode":1}]`
	got, err := ParsePS([]byte(in))
	if err != nil {
		t.Fatalf("ParsePS: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[1].State != "exited" || got[1].ExitCode != 1 {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestParsePSEmpty(t *testing.T) {
	for _, in := range []string{"", "\n"} {
		got, err := ParsePS([]byte(in))
		if err != nil {
			t.Fatalf("ParsePS(%q): %v", in, err)
		}
		if len(got) != 0 {
			t.Errorf("ParsePS(%q) len = %d, want 0", in, len(got))
		}
		if got != nil {
			t.Errorf("ParsePS(%q) = %v, want nil", in, got)
		}
	}
}

func TestParsePSInvalid(t *testing.T) {
	_, err := ParsePS([]byte("not json"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "parse compose ps output") {
		t.Errorf("err = %v", err)
	}
}

func TestServices(t *testing.T) {
	r := &fakeRunner{stdout: []byte("rclone-mount\nsuperdufs\n")}
	c := newTestClient(r)
	got, err := c.Services(context.Background())
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(got) != 2 || got[0] != "rclone-mount" || got[1] != "superdufs" {
		t.Errorf("got = %v", got)
	}
}

func wantBase() []string {
	return []string{"compose", "-f", "/proj/compose.yml", "--project-directory", "/proj"}
}

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
}

func TestPSArgs(t *testing.T) {
	r := &fakeRunner{}
	c := newTestClient(r)
	if _, err := c.PS(context.Background()); err != nil {
		t.Fatalf("PS: %v", err)
	}
	want := append(wantBase(), "ps", "-a", "--format", "json")
	assertArgs(t, r.args, want)
	if r.timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", r.timeout)
	}
}

func TestDownArgs(t *testing.T) {
	r := &fakeRunner{}
	c := newTestClient(r)
	if err := c.Down(context.Background(), 30*time.Second); err != nil {
		t.Fatalf("Down: %v", err)
	}
	want := append(wantBase(), "down", "-t", "30")
	assertArgs(t, r.args, want)
	if r.timeout != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", r.timeout)
	}
}

func TestUpArgs(t *testing.T) {
	r := &fakeRunner{}
	c := newTestClient(r)
	if err := c.Up(context.Background(), 120*time.Second); err != nil {
		t.Fatalf("Up: %v", err)
	}
	want := append(wantBase(), "up", "-d", "--pull", "never", "--wait", "--wait-timeout", "120")
	assertArgs(t, r.args, want)
	if r.timeout != 180*time.Second {
		t.Errorf("timeout = %v, want 180s", r.timeout)
	}
}

func TestKillArgs(t *testing.T) {
	r := &fakeRunner{}
	c := newTestClient(r)
	if err := c.Kill(context.Background()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	want := append(wantBase(), "kill")
	assertArgs(t, r.args, want)
	if r.timeout != 60*time.Second {
		t.Errorf("timeout = %v, want 60s", r.timeout)
	}
}

func TestExecArgs(t *testing.T) {
	r := &fakeRunner{}
	c := newTestClient(r)
	if _, err := c.Exec(context.Background(), 5*time.Second, "superdufs", "ls", "-A", "--", "/data"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := []string{"exec", "superdufs", "ls", "-A", "--", "/data"}
	assertArgs(t, r.args, want)
	if r.name != "docker" {
		t.Errorf("name = %q, want docker", r.name)
	}
	if r.timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", r.timeout)
	}
}

func TestRunErrorIncludesStderr(t *testing.T) {
	r := &fakeRunner{stderr: []byte("boom"), err: errors.New("exit status 1")}
	c := newTestClient(r)
	err := c.Down(context.Background(), 30*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "compose down") {
		t.Errorf("err = %v", err)
	}
}

func TestRunErrorKeepsStderrTail(t *testing.T) {
	// Compose prints progress first and the actual error last.
	stderr := strings.Repeat(" Container x Starting\n", 100) + "dependency failed to start: container rclone-mount is unhealthy\n"
	r := &fakeRunner{stderr: []byte(stderr), err: errors.New("exit status 1")}
	c := newTestClient(r)
	err := c.Up(context.Background(), 120*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "container rclone-mount is unhealthy") {
		t.Errorf("err = %q, want the last stderr line", msg)
	}
	if !strings.Contains(msg, "compose up") {
		t.Errorf("err = %q, want to contain %q", msg, "compose up")
	}
}

func TestRunErrorTruncatesStderr(t *testing.T) {
	r := &fakeRunner{stderr: []byte(strings.Repeat("x", 2000)), err: errors.New("exit status 1")}
	c := newTestClient(r)
	err := c.Down(context.Background(), 30*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if strings.Contains(msg, strings.Repeat("x", maxStderr+1)) {
		t.Errorf("err contains more than %d bytes of stderr: len=%d", maxStderr, len(msg))
	}
	if !strings.Contains(msg, strings.Repeat("x", maxStderr)) {
		t.Errorf("err = %q, want %d bytes of stderr", msg, maxStderr)
	}
	if !strings.Contains(msg, "down") {
		t.Errorf("err = %q, want to contain %q", msg, "down")
	}
}

func TestStderrTailRuneBoundary(t *testing.T) {
	// "é" is 2 bytes: a naive byte cut can split it.
	s := strings.Repeat("é", maxStderr)
	got := stderrTail([]byte(s))
	if !utf8.ValidString(got) {
		t.Errorf("stderrTail produced invalid UTF-8: %q", got[:10])
	}
	if !strings.HasPrefix(got, "...") {
		t.Errorf("stderrTail = %q..., want ... prefix", got[:10])
	}
}

func TestExecRunnerCapturesOutput(t *testing.T) {
	var r ExecRunner
	stdout, stderr, err := r.Run(context.Background(), 5*time.Second, "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(stdout) != "out\n" {
		t.Errorf("stdout = %q, want %q", stdout, "out\n")
	}
	if string(stderr) != "err\n" {
		t.Errorf("stderr = %q, want %q", stderr, "err\n")
	}
}

func TestExecRunnerTimeout(t *testing.T) {
	var r ExecRunner
	start := time.Now()
	_, _, err := r.Run(context.Background(), 200*time.Millisecond, "sleep", "5")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want timeout error", err)
	}
	if elapsed >= 3*time.Second {
		t.Errorf("elapsed = %v, want < 3s", elapsed)
	}
}
