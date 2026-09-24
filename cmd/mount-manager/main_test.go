package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
