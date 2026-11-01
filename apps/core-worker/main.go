// Command core-worker hosts the process-scoped MaaCore binding over versioned
// JSONL.
package main

import (
	"os"

	"github.com/dongwlin/rhodeck/apps/core-worker/internal/app"
)

// Set with -ldflags '-X main.version=...'. Local builds identify themselves.
var version = "dev"

func main() { os.Exit(app.Run(version)) }
