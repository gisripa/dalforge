// Command dalforge generates a Postgres data access layer from a protobuf IDL
// that declares access patterns.
package main

import (
	"context"
	"os"

	"github.com/gisripa/dalforge/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stderr))
}
