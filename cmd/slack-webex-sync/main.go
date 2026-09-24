// Command slack-webex-sync keeps paired Slack channels and Webex spaces in
// sync.
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

	"github.com/sthorne/slack-webex-sync/internal/config"
	"github.com/sthorne/slack-webex-sync/internal/secret"
	"github.com/sthorne/slack-webex-sync/internal/store"
	_ "github.com/sthorne/slack-webex-sync/internal/store/all" // register backends
)

var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: slack-webex-sync <command> [flags]

Commands:
  run                  Start syncing (default)
  check                Verify credentials and that both sides of every pairing are reachable
  webex-login          Authorize the Webex service account and store its tokens
  events list          Show events that failed repeatedly and were parked
  events retry ID      Put a parked event back in the queue (-all for every parked event)
  events drop ID       Delete a parked event
  generate-key         Print a new storage.encryption_key
  version              Print the version

Flags:
  -config path         Configuration file (default config.yaml)
  -debug               Debug logging
  -all                 With "events retry": retry every parked event
`)
}

type options struct {
	configPath string
	debug      bool
	all        bool
}

// parseArgs accepts flags before, between or after positional arguments.
func parseArgs(args []string) (options, []string) {
	var o options
	fs := flag.NewFlagSet("slack-webex-sync", flag.ExitOnError)
	fs.StringVar(&o.configPath, "config", "config.yaml", "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.BoolVar(&o.all, "all", false, "")
	fs.Usage = usage
	var positional []string
	for {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return o, positional
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func main() {
	opts, positional := parseArgs(os.Args[1:])
	cmd := "run"
	if len(positional) > 0 {
		cmd, positional = positional[0], positional[1:]
	}

	level := slog.LevelInfo
	if opts.debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	switch cmd {
	case "version":
		fmt.Println(version)
		return
	case "generate-key":
		key, err := secret.GenerateKey()
		if err != nil {
			fatal(cmd, err)
		}
		fmt.Println(key)
		return
	case "run", "check", "webex-login", "events":
	default:
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		fatal("load config", err)
	}
	box, err := secret.New(cfg.Storage.EncryptionKey)
	if err != nil {
		fatal("load config", fmt.Errorf("storage.encryption_key: %w", err))
	}
	st, err := store.Open(ctx, cfg.Storage.Driver, store.Options{
		DSN:       cfg.Storage.DSN,
		Retention: cfg.Storage.Retention(),
	})
	if err != nil {
		fatal("open storage", err)
	}
	defer st.Close()
	tokens := &tokenStore{st: st, box: box}

	switch cmd {
	case "run":
		err = run(ctx, cfg, st, tokens)
	case "check":
		err = check(ctx, cfg, tokens)
	case "webex-login":
		err = login(ctx, cfg, tokens)
	case "events":
		err = events(ctx, st, positional, opts.all)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fatal(cmd, err)
	}
}

func fatal(what string, err error) {
	slog.Error(what+" failed", "err", err)
	os.Exit(1)
}
