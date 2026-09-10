package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/munlucky/codex-account-pool/internal/app"
	processpkg "github.com/munlucky/codex-account-pool/internal/process"
	"github.com/munlucky/codex-account-pool/internal/profile"
)

var version = "dev"
var commit = "unknown"

func main() {
	root, err := profile.DefaultRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	application := app.New(
		profile.NewStore(root),
		processpkg.OSExecutor{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr},
		os.Stdout,
		os.Stderr,
		version,
	)
	application.Commit = commit
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := application.Execute(ctx, os.Args[1:]); err != nil {
		if !errors.Is(err, app.ErrUsage) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(2)
	}
}
