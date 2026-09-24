// Command mount-manager supervises the rclone mount + superdufs compose stack.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"mount-manager/internal/compose"
	"mount-manager/internal/config"
	"mount-manager/internal/health"
	"mount-manager/internal/mountinfo"
	"mount-manager/internal/probe"
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
	fmt.Fprintln(stderr, "check flags:")
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

	exe, _ := os.Executable()
	runner := &probe.Runner{Exe: exe, Timeout: cfg.CheckTimeout.Duration}
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

	deps := health.Deps{
		Mounts:  mountinfo.Read,
		Probe:   runner.Probe,
		Compose: composeClient,
		HTTP:    httpClient,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rep := health.New(cfg, deps).Evaluate(ctx)

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
