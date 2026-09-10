package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/munlucky/gpt-codex-router/internal/app"
	processpkg "github.com/munlucky/gpt-codex-router/internal/process"
	"github.com/munlucky/gpt-codex-router/internal/profile"
)

var version = "dev"

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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := application.Execute(ctx, os.Args[1:]); err != nil {
		if !errors.Is(err, app.ErrUsage) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(2)
	}
}
