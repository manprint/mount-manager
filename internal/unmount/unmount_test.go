//go:build linux

package unmount

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestAbortConnectionWritesFile(t *testing.T) {
	dir := t.TempDir()
	orig := ConnectionsDir
	ConnectionsDir = dir
	defer func() { ConnectionsDir = orig }()

	minor := 58
	connDir := filepath.Join(dir, strconv.Itoa(minor))
	if err := os.MkdirAll(connDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	abortPath := filepath.Join(connDir, "abort")
	if err := os.WriteFile(abortPath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := AbortConnection(minor); err != nil {
		t.Fatalf("AbortConnection: %v", err)
	}

	data, err := os.ReadFile(abortPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "1" {
		t.Errorf("abort file content = %q, want %q", string(data), "1")
	}
}

func TestAbortConnectionMissingIsNil(t *testing.T) {
	dir := t.TempDir()
	orig := ConnectionsDir
	ConnectionsDir = dir
	defer func() { ConnectionsDir = orig }()

	if err := AbortConnection(9999); err != nil {
		t.Errorf("AbortConnection for missing connection = %v, want nil", err)
	}
}

func TestClassifyDetachError(t *testing.T) {
	const path = "/mnt/mount"
	tests := []struct {
		name    string
		err     error
		wantNil bool
		wantSub string
		wantIs  error
	}{
		{name: "nil", err: nil, wantNil: true},
		{name: "EINVAL", err: syscall.EINVAL, wantNil: true},
		{name: "ENOENT", err: syscall.ENOENT, wantNil: true},
		{name: "EPERM", err: syscall.EPERM, wantSub: "root required"},
		{name: "EBUSY", err: syscall.EBUSY, wantSub: path, wantIs: syscall.EBUSY},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.err, path)
			if tt.wantNil {
				if got != nil {
					t.Errorf("classify(%v) = %v, want nil", tt.err, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("classify(%v) = nil, want error", tt.err)
			}
			if tt.wantSub != "" && !strings.Contains(got.Error(), tt.wantSub) {
				t.Errorf("classify(%v) = %q, want to contain %q", tt.err, got.Error(), tt.wantSub)
			}
			if tt.wantIs != nil && !errors.Is(got, tt.wantIs) {
				t.Errorf("classify(%v) = %v, want errors.Is match for %v", tt.err, got, tt.wantIs)
			}
		})
	}
}
