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

	"pdd/services/internal/gitgateway"
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

func (f *configFiles) String() string {
	return fmt.Sprint([]string(*f))
}

func (f *configFiles) Set(path string) error {
	*f = append(*f, path)
	return nil
}

func run(args []string) error {
	var files configFiles

	// "flag": cmd startup flag. ex: ./git-gateway --port 8080 --debug
	flags := flag.NewFlagSet("git-gateway", flag.ContinueOnError)
	// --config with type "var files configFiles"
	flags.Var(&files, "config", "YAML configuration `file` merged over the built-in defaults (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	// load the config file
	cfg, err := config.Load(files)
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}

	// create logger
	log := slog.New(
		slog.NewJSONHandler( // log format: JSON
			os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})) // std; minimum log level

	// create context signal for shutdown
	ctx, stop := signal.NotifyContext(
		context.Background(), // "Background": mean a blank context, not "background"
		os.Interrupt,         // shutdown source
		syscall.SIGTERM)
	defer stop() // stop when run() ended

	// create application (app.New)
	gateway, err := gitgateway.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer func() { _ = gateway.Close() }()

	return gateway.Run(ctx) // start the application
}
