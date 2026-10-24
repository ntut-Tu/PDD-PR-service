// Command git-gateway serves the pull request API of PDD. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"pdd/services/internal/gitgateway/app"
	"pdd/services/internal/gitgateway/config"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "git-gateway:", err)
		os.Exit(1)
	}
}

// configFiles is a repeatable flag; later files are merged over earlier ones.
type configFiles []string

func (f *configFiles) String() string { return fmt.Sprint([]string(*f)) }

func (f *configFiles) Set(path string) error {
	*f = append(*f, path)
	return nil
}

func run(args []string) error {
	var files configFiles
	flags := flag.NewFlagSet("git-gateway", flag.ContinueOnError)
	flags.Var(&files, "config", "YAML configuration `file` merged over the built-in defaults (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(files)
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	gateway, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer func() { _ = gateway.Close() }()
	return gateway.Run(ctx)
}
