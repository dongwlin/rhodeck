// core-worker hosts the process-scoped MaaCore binding over versioned JSONL.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() { os.Exit(run()) }

func run() int {
	flags := flag.NewFlagSet("core-worker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	showVersion := flags.Bool("version", false, "print the worker build version")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "core-worker: unexpected positional arguments")
		return 2
	}
	if *showVersion {
		fmt.Fprintln(os.Stdout, version)
		return 0
	}
	wire, err := isolateStdout()
	if err != nil {
		fmt.Fprintln(os.Stderr, "core-worker:", err)
		return 1
	}
	defer wire.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = serve(ctx, os.Stdin, wire, openEngine, shutdownGrace); err != nil {
		fmt.Fprintln(os.Stderr, "core-worker:", err)
		return 1
	}
	return 0
}
