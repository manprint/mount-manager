package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"mount-manager/internal/config"
)

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout.String() != "mount-manager dev\n" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "mount-manager dev\n")
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"nope"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !strings.Contains(stderr.String(), "usage") {
		t.Fatalf("stderr = %q, want to contain %q", stderr.String(), "usage")
	}
}

func TestNoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}

func TestProbeCommandOK(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"probe", "--path", dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, stderr = %q", code, stderr.String())
	}

	var res struct {
		OK         bool `json:"ok"`
		HasEntries bool `json:"has_entries"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("decode stdout %q: %s", stdout.String(), err)
	}
	if !res.OK || !res.HasEntries {
		t.Fatalf("res = %+v, want OK && HasEntries", res)
	}
}

func TestProbeCommandMissingPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"probe"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}

func TestCheckCommandConfigError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"check", "--config", "/nonexistent/config.yml"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if stderr.String() == "" {
		t.Fatalf("stderr is empty, want a config error")
	}
}

func TestRunCommandMissingConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--config", "/nonexistent/config.yml"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !strings.Contains(stderr.String(), "config") {
		t.Fatalf("stderr = %q, want to contain %q", stderr.String(), "config")
	}
}

func TestRunCommandInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("unknown_field: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--config", cfgPath}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if stderr.String() == "" {
		t.Fatalf("stderr is empty, want a config error")
	}
}

func TestRunCommandMissingDocker(t *testing.T) {
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgYAML := "mount_path: " + filepath.Join(dir, "mnt") + "\n" +
		"compose_file: " + composeFile + "\n" +
		"docker_binary: /nonexistent/docker\n"
	cfgPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--config", cfgPath}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3, stderr = %q", code, stderr.String())
	}
	if stderr.String() == "" {
		t.Fatalf("stderr is empty, want a docker lookup error")
	}
}

func TestUsageListsCommands(t *testing.T) {
	var stderr bytes.Buffer
	usage(&stderr)
	out := stderr.String()
	for _, cmd := range []string{"run", "check", "probe", "version"} {
		if !strings.Contains(out, cmd) {
			t.Fatalf("usage = %q, want to contain %q", out, cmd)
		}
	}
}

func TestFilePermWarnings(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	composePath := filepath.Join(dir, "compose.yml")
	for _, p := range []string{cfgPath, composePath} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withHook := &config.Config{ComposeFile: composePath, Webhook: config.Webhook{URL: "https://user:secret@example.invalid/x"}}
	noHook := &config.Config{ComposeFile: composePath}

	cases := []struct {
		name              string
		cfgMode, compMode os.FileMode
		cfg               *config.Config
		want              []string
	}{
		{"private", 0o600, 0o644, withHook, nil},
		{"readable without webhook", 0o644, 0o644, noHook, nil},
		{"readable with webhook", 0o644, 0o644, withHook, []string{"config readable"}},
		{"config group writable", 0o664, 0o644, noHook, []string{"config writable"}},
		{"compose other writable", 0o600, 0o646, withHook, []string{"compose writable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chmod(cfgPath, tc.cfgMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(composePath, tc.compMode); err != nil {
				t.Fatal(err)
			}
			got := filePermWarnings(cfgPath, tc.cfg)
			var kinds []string
			for _, w := range got {
				if strings.Contains(w, "secret") && strings.Contains(w, "example.invalid") {
					t.Errorf("warning leaks the webhook URL: %q", w)
				}
				file := "config"
				if strings.HasPrefix(w, "compose file") {
					file = "compose"
				}
				kind := "readable"
				if strings.Contains(w, "writable") {
					kind = "writable"
				}
				kinds = append(kinds, file+" "+kind)
			}
			if !slices.Equal(kinds, tc.want) {
				t.Errorf("warnings = %q, want kinds %v", got, tc.want)
			}
		})
	}
}

func TestShutdownSignals(t *testing.T) {
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP ignored by the test runner")
	}
	if !slices.Contains(shutdownSignals(), os.Signal(syscall.SIGHUP)) {
		t.Fatalf("shutdownSignals() = %v, want SIGHUP included", shutdownSignals())
	}
	signal.Ignore(syscall.SIGHUP)
	defer signal.Reset(syscall.SIGHUP)
	if slices.Contains(shutdownSignals(), os.Signal(syscall.SIGHUP)) {
		t.Fatalf("shutdownSignals() includes SIGHUP while it is ignored (nohup)")
	}
}
