// Command rhodeck-cli runs explicit MaaCore task plans through control.
package main

import (
	"os"

	"github.com/dongwlin/rhodeck/apps/cli/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Run(version))
}
