package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hanleo-6/mailcat/internal/config"
	"github.com/hanleo-6/mailcat/internal/convert"
	"github.com/hanleo-6/mailcat/internal/notify"
	"github.com/hanleo-6/mailcat/internal/server"
)

const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return exitUsage
	}

	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "serve":
		return runServe(args[1:])
	case "token":
		return runToken(args[1:])
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "mailcat: unknown command %q\n\n", args[0])
		printUsage(os.Stderr)
		return exitUsage
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: mailcat <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  init [--force]   Create config (0600) with a fresh token and directories")
	fmt.Fprintln(w, "  serve            Run the HTTP server in the foreground")
	fmt.Fprintln(w, "  token            Print the bearer token from the config")
}

func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	force := fs.Bool("force", false, "overwrite an existing config file")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "mailcat init: unexpected arguments")
		return exitUsage
	}

	cfgPath, err := config.Path()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailcat init: %v\n", err)
		return exitRuntime
	}

	cfg, err := config.Default()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailcat init: %v\n", err)
		return exitRuntime
	}

	tok, err := config.GenerateToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailcat init: %v\n", err)
		return exitRuntime
	}
	cfg.Token = tok

	if err := config.Write(cfgPath, cfg, *force); err != nil {
		if errors.Is(err, os.ErrExist) {
			fmt.Fprintf(os.Stderr, "mailcat init: config already exists at %s (use --force to overwrite)\n", cfgPath)
			return exitRuntime
		}
		fmt.Fprintf(os.Stderr, "mailcat init: %v\n", err)
		return exitRuntime
	}

	if err := os.MkdirAll(cfg.InboxDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mailcat init: create inbox_dir: %v\n", err)
		return exitRuntime
	}
	if err := os.MkdirAll(cfg.OutboxDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mailcat init: create outbox_dir: %v\n", err)
		return exitRuntime
	}

	fmt.Fprintf(os.Stderr, "wrote %s\n", cfgPath)
	fmt.Println(cfg.Token)
	return exitOK
}

func runToken(args []string) int {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "mailcat token: unexpected arguments")
		return exitUsage
	}

	cfgPath, err := config.Path()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailcat token: %v\n", err)
		return exitRuntime
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailcat token: %v\n", err)
		return exitRuntime
	}
	fmt.Println(cfg.Token)
	return exitOK
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "mailcat serve: unexpected arguments")
		return exitUsage
	}

	cfgPath, err := config.Path()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailcat serve: %v\n", err)
		return exitRuntime
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailcat serve: %v\n", err)
		return exitRuntime
	}

	logger := log.New(os.Stderr, "mailcat: ", log.LstdFlags)
	notifier := notify.New(logger)
	defer notifier.Close()

	pool := convert.New(cfg, notifier, logger)
	if pool != nil {
		defer pool.Close()
	}

	srv := server.New(cfg, logger)
	srv.SetNotifier(notifier)
	if pool != nil {
		srv.SetConverter(pool)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		logger.Printf("error: %v", err)
		return exitRuntime
	}
	return exitOK
}
