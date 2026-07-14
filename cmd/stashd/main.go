// Command stashd is a content-addressed artifact store for agent outputs:
// tags, retention policies, dedup, and garbage collection.
package main

import (
	"os"

	"github.com/JaydenCJ/stashd/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
