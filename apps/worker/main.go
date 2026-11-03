// Command worker hosts the process-scoped MaaCore binding over versioned
// JSONL.
package main

import (
	"os"

	"github.com/dongwlin/rhodeck/apps/worker/internal/app"
)

var version = "dev"

func main() { os.Exit(app.Run(version)) }
