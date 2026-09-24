package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupComposeFile creates a temp dir with a dummy compose.yml and returns
// the temp dir path and the compose file's absolute path.
func setupComposeFile(t *testing.T) (dir, composeFile string) {
	t.Helper()
	dir = t.TempDir()
	composeFile = filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(composeFile, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("write dummy compose.yml: %v", err)
	}
	return dir, composeFile
}

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	dir, composeFile := setupComposeFile(t)
	yamlSrc := fmt.Sprintf("mount_path: /mnt/mount\ncompose_file: %s\n", composeFile)

	cfg, err := LoadBytes([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	if cfg.MountPath != "/mnt/mount" {
		t.Errorf("MountPath = %q", cfg.MountPath)
	}
	if cfg.ComposeFile != composeFile {
		t.Errorf("ComposeFile = %q", cfg.ComposeFile)
	}
	if cfg.ProjectDirectory != dir {
		t.Errorf("ProjectDirectory = %q, want %q", cfg.ProjectDirectory, dir)
	}
	if cfg.DockerBinary != "docker" {
		t.Errorf("DockerBinary = %q", cfg.DockerBinary)
	}
	if cfg.ExpectedFSType != "fuse.rclone" {
		t.Errorf("ExpectedFSType = %q", cfg.ExpectedFSType)
	}
	if cfg.CheckInterval.Duration != 10*time.Second {
		t.Errorf("CheckInterval = %v", cfg.CheckInterval.Duration)
	}
	if cfg.CheckTimeout.Duration != 10*time.Second {
		t.Errorf("CheckTimeout = %v", cfg.CheckTimeout.Duration)
	}
	if cfg.FailureThreshold != 3 {
		t.Errorf("FailureThreshold = %d", cfg.FailureThreshold)
	}
	if cfg.StartupGrace.Duration != 120*time.Second {
		t.Errorf("StartupGrace = %v", cfg.StartupGrace.Duration)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.Recovery.DownTimeout.Duration != 30*time.Second {
		t.Errorf("Recovery.DownTimeout = %v", cfg.Recovery.DownTimeout.Duration)
	}
	if cfg.Recovery.UpWaitTimeout.Duration != 120*time.Second {
		t.Errorf("Recovery.UpWaitTimeout = %v", cfg.Recovery.UpWaitTimeout.Duration)
	}
	if cfg.Recovery.VerifyTimeout.Duration != 120*time.Second {
		t.Errorf("Recovery.VerifyTimeout = %v", cfg.Recovery.VerifyTimeout.Duration)
	}
	if cfg.Recovery.CommandGrace.Duration != 60*time.Second {
		t.Errorf("Recovery.CommandGrace = %v", cfg.Recovery.CommandGrace.Duration)
	}
	if cfg.Recovery.BackoffInitial.Duration != 10*time.Second {
		t.Errorf("Recovery.BackoffInitial = %v", cfg.Recovery.BackoffInitial.Duration)
	}
	if cfg.Recovery.BackoffMax.Duration != 5*time.Minute {
		t.Errorf("Recovery.BackoffMax = %v", cfg.Recovery.BackoffMax.Duration)
	}
	if cfg.Checks.HTTP.Timeout.Duration != 5*time.Second {
		t.Errorf("Checks.HTTP.Timeout = %v", cfg.Checks.HTTP.Timeout.Duration)
	}
	if len(cfg.Checks.HTTP.AcceptStatus) != 3 || cfg.Checks.HTTP.AcceptStatus[0] != 200 ||
		cfg.Checks.HTTP.AcceptStatus[1] != 401 || cfg.Checks.HTTP.AcceptStatus[2] != 403 {
		t.Errorf("Checks.HTTP.AcceptStatus = %v", cfg.Checks.HTTP.AcceptStatus)
	}
	if cfg.Checks.Exec.Path != "" {
		t.Errorf("Checks.Exec.Path = %q, want empty (service unset)", cfg.Checks.Exec.Path)
	}
	if cfg.Checks.Exec.Timeout.Duration != 10*time.Second {
		t.Errorf("Checks.Exec.Timeout = %v", cfg.Checks.Exec.Timeout.Duration)
	}
	if cfg.Webhook.Timeout.Duration != 5*time.Second {
		t.Errorf("Webhook.Timeout = %v", cfg.Webhook.Timeout.Duration)
	}
}

func TestLoadFullOverrides(t *testing.T) {
	dir, composeFile := setupComposeFile(t)

	yamlSrc := fmt.Sprintf(`
mount_path: /mnt/mount
compose_file: %s
project_directory: %s
docker_binary: /usr/bin/docker
expected_fstype: fuse.custom
check_interval: 1m30s
check_timeout: 20s
failure_threshold: 5
startup_grace: 200s
log_level: debug
recovery:
  down_timeout: 40s
  up_wait_timeout: 150s
  verify_timeout: 160s
  command_grace: 70s
  backoff_initial: 15s
  backoff_max: 6m
checks:
  canary: marker.txt
  http:
    url: https://example.com/health
    timeout: 8s
    accept_status: [200, 204]
  exec:
    service: superdufs
    path: /data/custom
    timeout: 12s
webhook:
  url: https://hooks.example.com/notify
  headers:
    X-Token: secret
  timeout: 9s
`, composeFile, dir)

	cfg, err := LoadBytes([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}

	if cfg.MountPath != "/mnt/mount" {
		t.Errorf("MountPath = %q", cfg.MountPath)
	}
	if cfg.ComposeFile != composeFile {
		t.Errorf("ComposeFile = %q", cfg.ComposeFile)
	}
	if cfg.ProjectDirectory != dir {
		t.Errorf("ProjectDirectory = %q", cfg.ProjectDirectory)
	}
	if cfg.DockerBinary != "/usr/bin/docker" {
		t.Errorf("DockerBinary = %q", cfg.DockerBinary)
	}
	if cfg.ExpectedFSType != "fuse.custom" {
		t.Errorf("ExpectedFSType = %q", cfg.ExpectedFSType)
	}
	if cfg.CheckInterval.Duration != 90*time.Second {
		t.Errorf("CheckInterval = %v, want 90s", cfg.CheckInterval.Duration)
	}
	if cfg.CheckTimeout.Duration != 20*time.Second {
		t.Errorf("CheckTimeout = %v", cfg.CheckTimeout.Duration)
	}
	if cfg.FailureThreshold != 5 {
		t.Errorf("FailureThreshold = %d", cfg.FailureThreshold)
	}
	if cfg.StartupGrace.Duration != 200*time.Second {
		t.Errorf("StartupGrace = %v", cfg.StartupGrace.Duration)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.Recovery.DownTimeout.Duration != 40*time.Second {
		t.Errorf("Recovery.DownTimeout = %v", cfg.Recovery.DownTimeout.Duration)
	}
	if cfg.Recovery.UpWaitTimeout.Duration != 150*time.Second {
		t.Errorf("Recovery.UpWaitTimeout = %v", cfg.Recovery.UpWaitTimeout.Duration)
	}
	if cfg.Recovery.VerifyTimeout.Duration != 160*time.Second {
		t.Errorf("Recovery.VerifyTimeout = %v", cfg.Recovery.VerifyTimeout.Duration)
	}
	if cfg.Recovery.CommandGrace.Duration != 70*time.Second {
		t.Errorf("Recovery.CommandGrace = %v", cfg.Recovery.CommandGrace.Duration)
	}
	if cfg.Recovery.BackoffInitial.Duration != 15*time.Second {
		t.Errorf("Recovery.BackoffInitial = %v", cfg.Recovery.BackoffInitial.Duration)
	}
	if cfg.Recovery.BackoffMax.Duration != 6*time.Minute {
		t.Errorf("Recovery.BackoffMax = %v", cfg.Recovery.BackoffMax.Duration)
	}
	if cfg.Checks.Canary != "marker.txt" {
		t.Errorf("Checks.Canary = %q", cfg.Checks.Canary)
	}
	if cfg.Checks.HTTP.URL != "https://example.com/health" {
		t.Errorf("Checks.HTTP.URL = %q", cfg.Checks.HTTP.URL)
	}
	if cfg.Checks.HTTP.Timeout.Duration != 8*time.Second {
		t.Errorf("Checks.HTTP.Timeout = %v", cfg.Checks.HTTP.Timeout.Duration)
	}
	if len(cfg.Checks.HTTP.AcceptStatus) != 2 || cfg.Checks.HTTP.AcceptStatus[0] != 200 || cfg.Checks.HTTP.AcceptStatus[1] != 204 {
		t.Errorf("Checks.HTTP.AcceptStatus = %v", cfg.Checks.HTTP.AcceptStatus)
	}
	if cfg.Checks.Exec.Service != "superdufs" {
		t.Errorf("Checks.Exec.Service = %q", cfg.Checks.Exec.Service)
	}
	if cfg.Checks.Exec.Path != "/data/custom" {
		t.Errorf("Checks.Exec.Path = %q", cfg.Checks.Exec.Path)
	}
	if cfg.Checks.Exec.Timeout.Duration != 12*time.Second {
		t.Errorf("Checks.Exec.Timeout = %v", cfg.Checks.Exec.Timeout.Duration)
	}
	if cfg.Webhook.URL != "https://hooks.example.com/notify" {
		t.Errorf("Webhook.URL = %q", cfg.Webhook.URL)
	}
	if cfg.Webhook.Headers["X-Token"] != "secret" {
		t.Errorf("Webhook.Headers = %v", cfg.Webhook.Headers)
	}
	if cfg.Webhook.Timeout.Duration != 9*time.Second {
		t.Errorf("Webhook.Timeout = %v", cfg.Webhook.Timeout.Duration)
	}
}

func TestLoadUnknownFieldRejected(t *testing.T) {
	_, composeFile := setupComposeFile(t)
	yamlSrc := fmt.Sprintf("mount_pth: /mnt/mount\ncompose_file: %s\n", composeFile)

	_, err := LoadBytes([]byte(yamlSrc))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "mount_pth") {
		t.Errorf("error = %q, want substring %q", err.Error(), "mount_pth")
	}
}

func TestLoadInvalidDuration(t *testing.T) {
	_, composeFile := setupComposeFile(t)
	yamlSrc := fmt.Sprintf("mount_path: /mnt/mount\ncompose_file: %s\ncheck_interval: 10x\n", composeFile)

	_, err := LoadBytes([]byte(yamlSrc))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid duration") {
		t.Errorf("error = %q, want substring %q", err.Error(), "invalid duration")
	}
}

func TestLoadEmptyDocument(t *testing.T) {
	_, err := LoadBytes([]byte(""))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "config: empty document" {
		t.Errorf("error = %q, want %q", err.Error(), "config: empty document")
	}
}

// validConfig returns a Config that passes Validate() unmodified, backed by
// a temp dir holding a dummy compose.yml.
func validConfig(t *testing.T) *Config {
	t.Helper()
	dir, composeFile := setupComposeFile(t)
	cfg := &Config{
		MountPath:        "/mnt/mount",
		ComposeFile:      composeFile,
		ProjectDirectory: dir,
		DockerBinary:     "docker",
		ExpectedFSType:   "fuse.rclone",
		CheckInterval:    Duration{10 * time.Second},
		CheckTimeout:     Duration{10 * time.Second},
		FailureThreshold: 3,
		StartupGrace:     Duration{120 * time.Second},
		LogLevel:         "info",
		Recovery: Recovery{
			DownTimeout:    Duration{30 * time.Second},
			UpWaitTimeout:  Duration{120 * time.Second},
			VerifyTimeout:  Duration{120 * time.Second},
			CommandGrace:   Duration{60 * time.Second},
			BackoffInitial: Duration{10 * time.Second},
			BackoffMax:     Duration{5 * time.Minute},
		},
		Checks: Checks{
			HTTP: HTTPCheck{
				Timeout:      Duration{5 * time.Second},
				AcceptStatus: []int{200, 401, 403},
			},
			Exec: ExecCheck{
				Timeout: Duration{10 * time.Second},
			},
		},
		Webhook: Webhook{
			Timeout: Duration{5 * time.Second},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("base valid config failed Validate: %v", err)
	}
	return cfg
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantSub string
	}{
		{"mount_path relative", func(c *Config) { c.MountPath = "mnt/mount" }, "mount_path"},
		{"mount_path root", func(c *Config) { c.MountPath = "/" }, "mount_path"},
		{"mount_path dotdot", func(c *Config) { c.MountPath = "/a/../b" }, "mount_path"},
		{"compose_file missing", func(c *Config) { c.ComposeFile = "/nonexistent/compose.yml" }, "compose_file"},
		{"canary absolute", func(c *Config) { c.Checks.Canary = "/abs" }, "checks.canary"},
		{"canary dotdot", func(c *Config) { c.Checks.Canary = "../x" }, "checks.canary"},
		{"canary dot", func(c *Config) { c.Checks.Canary = "." }, "checks.canary"},
		{"http url bad scheme", func(c *Config) { c.Checks.HTTP.URL = "ftp://h" }, "checks.http.url"},
		{"accept_status out of range", func(c *Config) { c.Checks.HTTP.AcceptStatus = []int{99} }, "checks.http.accept_status"},
		{"backoff_initial > backoff_max", func(c *Config) {
			c.Recovery.BackoffInitial = Duration{10 * time.Minute}
			c.Recovery.BackoffMax = Duration{5 * time.Minute}
		}, "backoff"},
		{"failure_threshold negative", func(c *Config) { c.FailureThreshold = -1 }, "failure_threshold"},
		{"log_level invalid", func(c *Config) { c.LogLevel = "trace" }, "log_level"},
		{"exec path relative", func(c *Config) {
			c.Checks.Exec.Service = "superdufs"
			c.Checks.Exec.Path = "data"
		}, "checks.exec.path"},
		{"webhook url invalid", func(c *Config) { c.Webhook.URL = "not a url" }, "webhook.url"},
		{"webhook header empty key", func(c *Config) { c.Webhook.Headers = map[string]string{"": "v"} }, "webhook.headers"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidateAggregatesErrors(t *testing.T) {
	cfg := validConfig(t)
	cfg.MountPath = "mnt/mount"
	cfg.LogLevel = "trace"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mount_path") {
		t.Errorf("error = %q, missing mount_path", msg)
	}
	if !strings.Contains(msg, "log_level") {
		t.Errorf("error = %q, missing log_level", msg)
	}
}
