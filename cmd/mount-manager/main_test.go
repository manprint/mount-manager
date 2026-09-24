package main

import (
	"bytes"
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
