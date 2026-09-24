// Package compose wraps invocations of `docker compose` for the managed
// mount stack.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mount-manager/internal/config"
)

// Container is one row of `docker compose ps -a --format json`.
type Container struct {
	Name     string `json:"Name"`
	Service  string `json:"Service"`
	State    string `json:"State"`
	Health   string `json:"Health"`
	ExitCode int    `json:"ExitCode"`
}

// Client drives `docker compose` for a single project.
type Client struct {
	Runner     Runner
	Docker     string
	File       string
	ProjectDir string
	Grace      time.Duration
}

// NewClient builds a Client from cfg, using ExecRunner as the Runner.
func NewClient(cfg *config.Config) *Client {
	return &Client{
		Runner:     ExecRunner{},
		Docker:     cfg.DockerBinary,
		File:       cfg.ComposeFile,
		ProjectDir: cfg.ProjectDirectory,
		Grace:      cfg.Recovery.CommandGrace.Duration,
	}
}

// secs renders d as whole seconds for CLI flags, minimum "0".
func secs(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s < 0 {
		s = 0
	}
	return strconv.Itoa(s)
}

func (c *Client) base() []string {
	return []string{"compose", "-f", c.File, "--project-directory", c.ProjectDir}
}

// run executes sub with the given full args and timeout, wrapping any error
// with the subcommand name and truncated stderr.
func (c *Client) run(ctx context.Context, timeout time.Duration, sub string, args ...string) ([]byte, error) {
	stdout, stderr, err := c.Runner.Run(ctx, timeout, c.Docker, args...)
	if err != nil {
		trimmed := strings.TrimSpace(string(stderr))
		if len(trimmed) > 500 {
			trimmed = trimmed[:500]
		}
		return stdout, fmt.Errorf("docker compose %s: %w: %s", sub, err, trimmed)
	}
	return stdout, nil
}

// Services returns the list of services declared by the compose file.
func (c *Client) Services(ctx context.Context) ([]string, error) {
	args := append(c.base(), "config", "--services")
	out, err := c.run(ctx, 30*time.Second, "config", args...)
	if err != nil {
		return nil, err
	}
	var services []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		services = append(services, line)
	}
	return services, nil
}

// PS returns the current containers of the project.
func (c *Client) PS(ctx context.Context) ([]Container, error) {
	args := append(c.base(), "ps", "-a", "--format", "json")
	out, err := c.run(ctx, 30*time.Second, "ps", args...)
	if err != nil {
		return nil, err
	}
	return ParsePS(out)
}

// Down stops and removes the project's containers, waiting up to stop
// seconds for graceful shutdown.
func (c *Client) Down(ctx context.Context, stop time.Duration) error {
	args := append(c.base(), "down", "-t", secs(stop))
	_, err := c.run(ctx, stop+c.Grace, "down", args...)
	return err
}

// Kill force-stops the project's containers.
func (c *Client) Kill(ctx context.Context) error {
	args := append(c.base(), "kill")
	_, err := c.run(ctx, c.Grace, "kill", args...)
	return err
}

// Up starts the project's containers, always disallowing image pulls (I-5).
func (c *Client) Up(ctx context.Context, wait time.Duration) error {
	args := append(c.base(), "up", "-d", "--pull", "never", "--wait", "--wait-timeout", secs(wait))
	_, err := c.run(ctx, wait+c.Grace, "up", args...)
	return err
}

// Exec runs cmd inside service's container without a TTY.
func (c *Client) Exec(ctx context.Context, timeout time.Duration, service string, cmd ...string) ([]byte, error) {
	args := append(c.base(), "exec", "-T", service)
	args = append(args, cmd...)
	return c.run(ctx, timeout, "exec", args...)
}

// ParsePS parses the output of `docker compose ps -a --format json`, which
// may be NDJSON (one object per line), a JSON array, or empty.
func ParsePS(b []byte) ([]Container, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil, nil
	}
	if b[0] == '[' {
		var containers []Container
		if err := json.Unmarshal(b, &containers); err != nil {
			return nil, fmt.Errorf("parse compose ps output: %w", err)
		}
		return containers, nil
	}
	var containers []Container
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ct Container
		if err := json.Unmarshal([]byte(line), &ct); err != nil {
			return nil, fmt.Errorf("parse compose ps output: %w", err)
		}
		containers = append(containers, ct)
	}
	return containers, nil
}
