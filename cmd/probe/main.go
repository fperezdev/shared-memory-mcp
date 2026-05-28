// probe-policy reads __policy observations from the local cache and
// prints them. Used to verify what's actually in the cache after a sync.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/fperez/shared-memory-mcp/internal/local"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe <local.db>")
		os.Exit(2)
	}
	db, err := local.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()

	defaultsID := local.ProjectID("__defaults")
	obs, err := local.PolicyObservations(context.Background(), db, defaultsID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("__defaults/__policy: %d observation(s)\n", len(obs))
	for i, o := range obs {
		fmt.Printf("  %d. %s\n", i+1, o)
	}
}
