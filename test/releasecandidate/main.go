// Command releasecandidate verifies signed draft release assets and assembles
// the release-candidate-v1.json evidence index consumed by the release
// workflow. It is source-bound tooling: the release workflow runs it from the
// clean tag checkout with `go run ./test/releasecandidate <subcommand>`.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: releasecandidate <verify-assets|manifest> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "verify-assets":
		err = runVerifyAssets(os.Args[2:])
	case "manifest":
		err = runManifest(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		var exit exitError
		if asExit(err, &exit) {
			fmt.Fprintln(os.Stderr, exit.Error())
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// exitError carries a specific process exit code through the subcommand
// boundary. Subcommands return it when a non-1 status is part of their
// contract (for example the manifest's "written but not approved" status).
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

func asExit(err error, target *exitError) bool {
	e, ok := err.(exitError)
	if ok {
		*target = e
	}
	return ok
}
