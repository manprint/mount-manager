// Command mount-manager supervises the rclone mount + superdufs compose stack.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

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
	fmt.Fprintln(stderr, "  check    run a single mount check")
	fmt.Fprintln(stderr, "  probe    probe a mount point from a subprocess")
	fmt.Fprintln(stderr, "  version  print the version")
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
