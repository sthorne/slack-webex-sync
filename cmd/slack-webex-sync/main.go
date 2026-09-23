// Command slack-webex-sync keeps paired Slack channels and Webex spaces in
// sync.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/sthorne/slack-webex-sync/internal/bridge"
	"github.com/sthorne/slack-webex-sync/internal/config"
	"github.com/sthorne/slack-webex-sync/internal/slackapi"
	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/webex"
)

const (
	tokensKey = "webex.tokens"
	deviceKey = "webex.device_url"
)

var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: slack-webex-sync <command> [-config path]

Commands:
  run          Start syncing (default)
  check        Verify credentials and that both sides of every pairing are reachable
  webex-login  Authorize the Webex service account and store its tokens
  version      Print the version
`)
}

func main() {
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to the configuration file")
	debug := fs.Bool("debug", false, "enable debug logging")
	fs.Usage = usage
	_ = fs.Parse(args)

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if cmd == "version" {
		fmt.Println(version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	st, err := store.Open(ctx, cfg.Storage.Driver, cfg.Storage.DSN)
	if err != nil {
		fatal("open storage", err)
	}
	defer st.Close()

	switch cmd {
	case "run":
		err = run(ctx, cfg, st)
	case "check":
		err = check(ctx, cfg, st)
	case "webex-login":
		err = login(ctx, cfg, st)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fatal(cmd, err)
	}
}

func fatal(what string, err error) {
	slog.Error(what+" failed", "err", err)
	os.Exit(1)
}

// webexClient builds a Webex client, preferring tokens saved by an earlier
// refresh or webex-login over the ones in the config file.
func webexClient(ctx context.Context, cfg *config.Config, st store.Store) *webex.Client {
	tokens := webex.Tokens{AccessToken: cfg.Webex.AccessToken, RefreshToken: cfg.Webex.RefreshToken}
	if saved, err := st.GetValue(ctx, tokensKey); err == nil && saved != "" {
		var stored webex.Tokens
		if json.Unmarshal([]byte(saved), &stored) == nil && stored.RefreshToken != "" {
			tokens = stored
		}
	}
	return webex.NewClient(webex.Options{
		ClientID:     cfg.Webex.ClientID,
		ClientSecret: cfg.Webex.ClientSecret,
		Tokens:       tokens,
		OnRefresh: func(t webex.Tokens) {
			saveTokens(context.Background(), st, t)
		},
	})
}

func saveTokens(ctx context.Context, st store.Store, t webex.Tokens) {
	data, _ := json.Marshal(t)
	if err := st.SetValue(ctx, tokensKey, string(data)); err != nil {
		slog.Error("could not save webex tokens", "err", err)
	}
}

func run(ctx context.Context, cfg *config.Config, st store.Store) error {
	wx := webexClient(ctx, cfg, st)
	sl := slackapi.New(cfg.Slack.BotToken, cfg.Slack.AppToken)
	b := bridge.New(cfg, st, sl, wx)
	if err := b.Start(ctx); err != nil {
		return err
	}

	rooms := make([]string, 0, len(cfg.Pairings))
	for _, p := range cfg.Pairings {
		rooms = append(rooms, p.WebexRoom)
	}
	source := &webex.Source{
		Poller:   webex.NewPoller(wx, rooms, b.WebexSelfID()),
		Interval: cfg.Webex.PollInterval,
	}
	if cfg.Webex.WebsocketEnabled() {
		listener := webex.NewListener(wx, cfg.Webex.DeviceURL, rooms, b.WebexSelfID())
		listener.LoadDevice = func() string {
			v, _ := st.GetValue(context.Background(), deviceKey)
			return v
		}
		listener.SaveDevice = func(url string) {
			if err := st.SetValue(context.Background(), deviceKey, url); err != nil {
				slog.Warn("could not save webex device", "err", err)
			}
		}
		source.Listener = listener
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.Run(ctx) }()
	go func() { defer wg.Done(); source.Run(ctx, b.SubmitWebex) }()
	err := sl.Listen(ctx, b.SubmitSlack)
	cancel() // if Slack stops, stop everything else too
	wg.Wait()
	return err
}

func check(ctx context.Context, cfg *config.Config, st store.Store) error {
	sl := slackapi.New(cfg.Slack.BotToken, cfg.Slack.AppToken)
	wx := webexClient(ctx, cfg, st)

	_, botUser, err := sl.Identity(ctx)
	if err != nil {
		return fmt.Errorf("slack auth: %w", err)
	}
	fmt.Printf("Slack bot user:      %s\n", botUser)
	me, err := wx.Me(ctx)
	if err != nil {
		return fmt.Errorf("webex auth: %w", err)
	}
	fmt.Printf("Webex account:       %s <%s>\n", me.DisplayName, me.Email)
	if me.IsBot {
		fmt.Println("  WARNING: this is a bot account; it will only see messages that @mention it.")
	}

	problems := 0
	for _, p := range cfg.Pairings {
		fmt.Printf("\nPairing %q\n", p.Name)
		if name, member, err := sl.ChannelStatus(ctx, p.SlackChannel); err != nil {
			problems++
			fmt.Printf("  slack  %s: ERROR %v\n", p.SlackChannel, err)
		} else if !member {
			problems++
			fmt.Printf("  slack  #%s: bot is NOT a member (invite it with /invite)\n", name)
		} else {
			fmt.Printf("  slack  #%s: ok\n", name)
		}
		if room, err := wx.Room(ctx, p.WebexRoom); err != nil {
			problems++
			fmt.Printf("  webex  %s: ERROR %v\n", p.WebexRoom, err)
		} else {
			fmt.Printf("  webex  %v: ok\n", room["title"])
		}
	}
	if problems > 0 {
		return fmt.Errorf("%d problem(s) found", problems)
	}
	fmt.Println("\nAll pairings look good.")
	return nil
}

func login(ctx context.Context, cfg *config.Config, st store.Store) error {
	wx := webex.NewClient(webex.Options{ClientID: cfg.Webex.ClientID, ClientSecret: cfg.Webex.ClientSecret})
	tokens, err := webex.Login(ctx, wx, cfg.Webex.ClientID, cfg.Webex.RedirectURI, cfg.Webex.Scopes, func(url string) {
		fmt.Println("Sign in as the Webex service account and open this URL:")
		fmt.Println()
		fmt.Println("  " + url)
		fmt.Println()
		fmt.Println("Waiting for the redirect to", cfg.Webex.RedirectURI, "...")
	})
	if err != nil {
		return err
	}
	saveTokens(ctx, st, tokens)
	me, err := wx.Me(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nAuthorized as %s <%s>. Tokens saved to %s storage.\n", me.DisplayName, me.Email, cfg.Storage.Driver)
	fmt.Println("They are refreshed automatically; you do not need to put them in the config file.")
	return nil
}
