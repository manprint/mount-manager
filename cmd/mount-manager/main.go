// Command mount-manager supervises the rclone mount + superdufs compose stack.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"mount-manager/internal/compose"
	"mount-manager/internal/config"
	"mount-manager/internal/health"
	"mount-manager/internal/mountinfo"
	"mount-manager/internal/notify"
	"mount-manager/internal/probe"
	"mount-manager/internal/recovery"
	"mount-manager/internal/supervisor"
	"mount-manager/internal/unmount"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func usage(stderr io.Writer) {
	fmt.Fprintln(stderr, "usage: mount-manager <command> [arguments]")
	fmt.Fprintln(stderr, "commands:")
	fmt.Fprintln(stderr, "  run      supervise the mount and the compose stack")
	fmt.Fprintln(stderr, "  check    run the health-check suite once")
	fmt.Fprintln(stderr, "  probe    probe a mount point from a subprocess")
	fmt.Fprintln(stderr, "  version  print the version")
	fmt.Fprintln(stderr, "")
	fmt.Fprintln(stderr, "run/check flags:")
	fmt.Fprintln(stderr, "  --config P   path to the configuration file (default "+config.DefaultPath+")")
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 3
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "mount-manager %s\n", version)
		return 0
	case "probe":
		return runProbe(args[1:], stdout, stderr)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "run":
		return runRun(args[1:], stdout, stderr)
	default:
		usage(stderr)
		return 3
	}
}

// runProbe implements the "probe" subcommand: it checks a mount point in
// this process and prints the JSON result to stdout. It is meant to be
// invoked as a short-lived subprocess by internal/probe.Runner, so that a
// hung mount cannot block the calling process.
func runProbe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("path", "", "mount point to probe")
	canary := fs.String("canary", "", "optional file that must exist under path")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *path == "" {
		fmt.Fprintln(stderr, "usage: mount-manager probe --path P [--canary C]")
		return 3
	}

	res := probe.Local(*path, *canary)
	if err := json.NewEncoder(stdout).Encode(res); err != nil {
		fmt.Fprintln(stderr, err)
		return 3
	}
	return 0
}

// builtDeps holds the shared dependencies constructed from a loaded
// configuration, used by both the "check" and "run" subcommands.
type builtDeps struct {
	health  health.Deps
	compose *compose.Client
}

// selfExe re-executes this very binary for the probe subprocess, even after
// the file on disk was replaced by an upgrade or removed.
const selfExe = "/proc/self/exe"

// buildDeps assembles the health-check and compose dependencies shared by
// "check" and "run".
func buildDeps(cfg *config.Config) builtDeps {
	runner := &probe.Runner{Exe: selfExe, Timeout: cfg.CheckTimeout.Duration}
	composeClient := compose.NewClient(cfg)

	var httpClient *http.Client
	if cfg.Checks.HTTP.URL != "" {
		httpClient = &http.Client{
			Timeout: cfg.Checks.HTTP.Timeout.Duration,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	return builtDeps{
		health: health.Deps{
			Mounts:  mountinfo.Read,
			Probe:   runner.Probe,
			Compose: composeClient,
			HTTP:    httpClient,
		},
		compose: composeClient,
	}
}

// runCheck implements the "check" subcommand: it loads the configuration,
// runs the health-check suite once and prints a short status report.
func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", config.DefaultPath, "path to the configuration file")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 3
	}

	deps := buildDeps(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rep := health.New(cfg, deps.health).Evaluate(ctx)

	status := "failing"
	switch {
	case rep.OK:
		status = "ok"
	case rep.Idle:
		status = "idle"
	}
	fmt.Fprintf(stdout, "status: %s\n", status)
	for _, f := range rep.Failures {
		fmt.Fprintf(stdout, "- %s: %s\n", f.Check, f.Detail)
	}
	if rep.MountStale {
		fmt.Fprintln(stdout, "- mount: stale")
	}

	switch {
	case rep.OK:
		return 0
	case rep.Idle:
		return 2
	default:
		return 1
	}
}

// runRun implements the "run" subcommand: it loads the configuration and
// runs the supervisor loop until SIGINT or SIGTERM is received.
func runRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", config.DefaultPath, "path to the configuration file")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 3
	}

	if _, err := exec.LookPath(cfg.DockerBinary); err != nil {
		fmt.Fprintln(stderr, err)
		return 3
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevelValue()}))

	if os.Geteuid() != 0 {
		log.Warn("not running as root: unmount and fuse abort will fail")
	}

	deps := buildDeps(cfg)

	rec := recovery.New(cfg, recovery.Deps{
		Mounts:  mountinfo.Read,
		Abort:   unmount.AbortConnection,
		Detach:  unmount.Detach,
		Compose: deps.compose,
		Log:     log,
	})

	var webhook *notify.Webhook
	var notifier notify.Notifier
	if cfg.Webhook.URL != "" {
		webhook = notify.NewWebhook(cfg.Webhook.URL, cfg.Webhook.Headers, cfg.Webhook.Timeout.Duration, log)
		notifier = webhook
	} else {
		notifier = notify.Nop{}
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}

	sup := supervisor.New(supervisor.Options{
		Config:   cfg,
		Health:   health.New(cfg, deps.health),
		Recovery: rec,
		Notifier: notifier,
		Clock:    supervisor.RealClock{},
		Log:      log,
		Host:     host,
		Version:  version,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	_ = sup.Run(ctx)

	if webhook != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		webhook.Close(closeCtx)
	}

	return 0
}
