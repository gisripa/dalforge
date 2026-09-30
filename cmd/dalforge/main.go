// Command dalforge generates a Postgres data access layer from a protobuf IDL
// that declares access patterns.
package main

import (
	"os"

	"github.com/gisripa/dalforge/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stderr))
}
