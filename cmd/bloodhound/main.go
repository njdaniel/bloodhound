// Command bloodhound is the entrypoint for the bloodhound incident-response
// agent pack. See specs/001-build-spec.md for the architecture.
package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "dev"

const usage = `bloodhound — AI incident-response agent pack for Kubernetes

Usage:
  bloodhound serve    Run the Alertmanager webhook intake server
  bloodhound hunt     Investigate a single alert from a JSON file
  bloodhound replay   Re-run a recorded case from its work dir
  bloodhound cost     Show cost/latency breakdown for a case
  bloodhound version  Print version
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	switch flag.Arg(0) {
	case "serve", "hunt", "replay", "cost":
		fmt.Fprintf(os.Stderr, "bloodhound: %q is not implemented yet (M0 skeleton)\n", flag.Arg(0))
		os.Exit(1)
	case "version":
		fmt.Println(version)
	default:
		flag.Usage()
		os.Exit(2)
	}
}
