// Package config loads and validates the mount-manager YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultPath is the default location of the mount-manager configuration file.
const DefaultPath = "/etc/mount-manager/config.yml"

// Duration wraps time.Duration to decode from a YAML scalar such as "10s".
type Duration struct{ time.Duration }

// UnmarshalYAML decodes a scalar duration string via time.ParseDuration.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	d.Duration = v
	return nil
}

// Config is the top-level mount-manager configuration.
type Config struct {
	MountPath        string   `yaml:"mount_path"`
	ComposeFile      string   `yaml:"compose_file"`
	ProjectDirectory string   `yaml:"project_directory"`
	DockerBinary     string   `yaml:"docker_binary"`
	ExpectedFSType   string   `yaml:"expected_fstype"`
	CheckInterval    Duration `yaml:"check_interval"`
	CheckTimeout     Duration `yaml:"check_timeout"`
	FailureThreshold int      `yaml:"failure_threshold"`
	StartupGrace     Duration `yaml:"startup_grace"`
	LogLevel         string   `yaml:"log_level"`
	Recovery         Recovery `yaml:"recovery"`
	Checks           Checks   `yaml:"checks"`
	Webhook          Webhook  `yaml:"webhook"`
}

// Recovery holds the timeouts and backoff parameters used during recovery.
type Recovery struct {
	DownTimeout    Duration `yaml:"down_timeout"`
	UpWaitTimeout  Duration `yaml:"up_wait_timeout"`
	VerifyTimeout  Duration `yaml:"verify_timeout"`
	CommandGrace   Duration `yaml:"command_grace"`
	BackoffInitial Duration `yaml:"backoff_initial"`
	BackoffMax     Duration `yaml:"backoff_max"`
}

// Checks holds the health-check configuration.
type Checks struct {
	Canary string    `yaml:"canary"`
	HTTP   HTTPCheck `yaml:"http"`
	Exec   ExecCheck `yaml:"exec"`
}

// HTTPCheck configures an HTTP-based health check.
type HTTPCheck struct {
	URL          string   `yaml:"url"`
	Timeout      Duration `yaml:"timeout"`
	AcceptStatus []int    `yaml:"accept_status"`
}

// ExecCheck configures an exec-based health check run inside a container.
type ExecCheck struct {
	Service string   `yaml:"service"`
	Path    string   `yaml:"path"`
	Timeout Duration `yaml:"timeout"`
}

// Webhook configures the notification webhook.
type Webhook struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Timeout Duration          `yaml:"timeout"`
}

// Load reads the file at path and returns a validated Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return LoadBytes(data)
}

// LoadBytes decodes data as YAML with strict (known-fields-only) decoding
// on top of the defaults, fills the derived defaults and validates the
// result. A key that is absent keeps its default; an explicit zero value is
// kept as written and rejected by Validate where zero is not allowed.
func LoadBytes(data []byte) (*Config, error) {
	cfg := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config: empty document")
		}
		return nil, fmt.Errorf("config: %w", err)
	}

	cfg.applyDerivedDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// defaults returns a Config holding every default that does not depend on
// another field.
func defaults() *Config {
	return &Config{
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
}

// applyDerivedDefaults fills the defaults that depend on other fields.
func (c *Config) applyDerivedDefaults() {
	if c.ProjectDirectory == "" && c.ComposeFile != "" {
		c.ProjectDirectory = filepath.Dir(c.ComposeFile)
	}
	if c.Checks.Exec.Service != "" && c.Checks.Exec.Path == "" {
		c.Checks.Exec.Path = "/data"
	}
}

// Validate checks the configuration for consistency, returning a joined
// error listing every violation found.
func (c *Config) Validate() error {
	var errs []error
	addf := func(field, reason string) {
		errs = append(errs, fmt.Errorf("config: %s: %s", field, reason))
	}

	if c.MountPath == "" {
		addf("mount_path", "required")
	} else if !filepath.IsAbs(c.MountPath) {
		addf("mount_path", "must be absolute")
	} else if c.MountPath != filepath.Clean(c.MountPath) {
		addf("mount_path", "must be clean")
	} else if c.MountPath == "/" {
		addf("mount_path", "must not be /")
	} else if err := checkNoSymlink(c.MountPath); err != nil {
		addf("mount_path", err.Error())
	}

	if c.ComposeFile == "" {
		addf("compose_file", "required")
	} else if !filepath.IsAbs(c.ComposeFile) {
		addf("compose_file", "must be absolute")
	} else if fi, err := os.Stat(c.ComposeFile); err != nil {
		addf("compose_file", "stat failed: "+err.Error())
	} else if !fi.Mode().IsRegular() {
		addf("compose_file", "must be a regular file")
	}

	// An empty project_directory means compose_file is missing too (it is
	// derived from it), which is already reported.
	if c.ProjectDirectory != "" {
		if !filepath.IsAbs(c.ProjectDirectory) {
			addf("project_directory", "must be absolute")
		} else if fi, err := os.Stat(c.ProjectDirectory); err != nil {
			addf("project_directory", "stat failed: "+err.Error())
		} else if !fi.IsDir() {
			addf("project_directory", "must be a directory")
		}
	}

	if c.ExpectedFSType == "" {
		addf("expected_fstype", "must not be empty")
	}
	if c.DockerBinary == "" {
		addf("docker_binary", "must not be empty")
	}

	checkDur := func(field string, d Duration) {
		if d.Duration <= 0 {
			addf(field, "must be > 0")
		}
	}
	checkDur("check_interval", c.CheckInterval)
	checkDur("check_timeout", c.CheckTimeout)
	checkDur("startup_grace", c.StartupGrace)
	checkDur("recovery.down_timeout", c.Recovery.DownTimeout)
	checkDur("recovery.up_wait_timeout", c.Recovery.UpWaitTimeout)
	checkDur("recovery.verify_timeout", c.Recovery.VerifyTimeout)
	checkDur("recovery.command_grace", c.Recovery.CommandGrace)
	checkDur("recovery.backoff_initial", c.Recovery.BackoffInitial)
	checkDur("recovery.backoff_max", c.Recovery.BackoffMax)
	checkDur("checks.http.timeout", c.Checks.HTTP.Timeout)
	checkDur("checks.exec.timeout", c.Checks.Exec.Timeout)
	checkDur("webhook.timeout", c.Webhook.Timeout)

	if c.FailureThreshold < 1 {
		addf("failure_threshold", "must be >= 1")
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		addf("log_level", "must be one of debug, info, warn, error")
	}

	if c.Recovery.BackoffInitial.Duration > c.Recovery.BackoffMax.Duration {
		addf("recovery.backoff_initial", "must be <= recovery.backoff_max")
	}

	if c.Checks.Canary != "" {
		cv := c.Checks.Canary
		switch {
		case filepath.IsAbs(cv):
			addf("checks.canary", "must not be absolute")
		case cv != filepath.Clean(cv):
			addf("checks.canary", "must be clean")
		case cv == ".":
			addf("checks.canary", "must not be .")
		case strings.HasPrefix(cv, ".."):
			addf("checks.canary", "must not escape with ..")
		}
	}

	validateURL := func(field, raw string) {
		if raw == "" {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			addf(field, "invalid url: "+err.Error())
			return
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			addf(field, "scheme must be http or https")
			return
		}
		if u.Host == "" {
			addf(field, "host must not be empty")
		}
	}
	validateURL("checks.http.url", c.Checks.HTTP.URL)
	validateURL("webhook.url", c.Webhook.URL)

	if c.Checks.HTTP.URL != "" && len(c.Checks.HTTP.AcceptStatus) == 0 {
		addf("checks.http.accept_status", "must not be empty when checks.http.url is set")
	}
	for _, s := range c.Checks.HTTP.AcceptStatus {
		if s < 100 || s > 599 {
			addf("checks.http.accept_status", fmt.Sprintf("value %d out of range 100..599", s))
		}
	}

	if c.Checks.Exec.Service != "" && !strings.HasPrefix(c.Checks.Exec.Path, "/") {
		addf("checks.exec.path", "must be an absolute POSIX path")
	}

	for k := range c.Webhook.Headers {
		if k == "" {
			addf("webhook.headers", "keys must not be empty")
		}
	}

	return errors.Join(errs...)
}

// checkNoSymlink returns an error if p (absolute and clean) contains a
// symlink: mountinfo reports resolved paths, so a symlinked mount_path would
// never match and the mount would always look missing. It never stats p
// itself, which may be a hung FUSE mount (I-3): the parent is resolved with
// EvalSymlinks, and p is only passed to Readlink, which fails with EINVAL on
// a directory or mount root without a request to the filesystem. Paths that
// do not exist yet are accepted.
func checkNoSymlink(p string) error {
	parent := filepath.Dir(p)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil
	}
	if resolved != parent {
		return fmt.Errorf("must not contain symlinks (%s resolves to %s)", parent, resolved)
	}
	if target, err := os.Readlink(p); err == nil {
		return fmt.Errorf("must not be a symlink (points to %s)", target)
	}
	return nil
}

// LogLevelValue converts LogLevel to a slog.Level.
func (c *Config) LogLevelValue() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
