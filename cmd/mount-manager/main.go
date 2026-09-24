// Command mount-manager supervises the rclone mount + superdufs compose stack.
package main

import (
	"fmt"
	"io"
	"os"
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
	default:
		usage(stderr)
		return 3
	}
}
