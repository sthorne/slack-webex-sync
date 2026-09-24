package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/sthorne/slack-webex-sync/internal/bridge"
	"github.com/sthorne/slack-webex-sync/internal/config"
	"github.com/sthorne/slack-webex-sync/internal/slackapi"
	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/webex"
)

func check(ctx context.Context, cfg *config.Config, tokens *tokenStore) error {
	sl := slackapi.New(cfg.Slack.BotToken, cfg.Slack.AppToken)
	wx, err := webexClient(ctx, cfg, tokens)
	if err != nil {
		return err
	}

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

func login(ctx context.Context, cfg *config.Config, tokens *tokenStore) error {
	wx := webex.NewClient(webex.Options{ClientID: cfg.Webex.ClientID, ClientSecret: cfg.Webex.ClientSecret})
	tok, err := webex.Login(ctx, wx, cfg.Webex.ClientID, cfg.Webex.RedirectURI, cfg.Webex.Scopes, func(url string) {
		fmt.Println("Sign in as the Webex service account and open this URL:")
		fmt.Println()
		fmt.Println("  " + url)
		fmt.Println()
		fmt.Println("Waiting for the redirect to", cfg.Webex.RedirectURI, "...")
	})
	if err != nil {
		return err
	}
	if err := tokens.save(ctx, tok); err != nil {
		return fmt.Errorf("save tokens: %w", err)
	}
	me, err := wx.Me(ctx)
	if err != nil {
		return err
	}
	how := "unencrypted"
	if tokens.box.Enabled() {
		how = "encrypted"
	}
	fmt.Printf("\nAuthorized as %s <%s>. Tokens saved (%s) to %s storage.\n", me.DisplayName, me.Email, how, cfg.Storage.Driver)
	fmt.Println("They are refreshed automatically; you do not need to put them in the config file.")
	return nil
}

const parkedListLimit = 1000

// events implements "events list", "events retry ID|-all" and "events drop ID".
func events(ctx context.Context, st store.Store, args []string, all bool) error {
	if len(args) == 0 {
		return errors.New("usage: events list | events retry ID | events retry -all | events drop ID")
	}
	switch args[0] {
	case "list":
		parked, err := st.ParkedEvents(ctx, parkedListLimit)
		if err != nil {
			return err
		}
		pending, total, err := st.CountEvents(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%d parked, %d pending\n", total, pending)
		if len(parked) == 0 {
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "\nID\tQUEUED\tATTEMPTS\tEVENT\tLAST ERROR")
		for _, e := range parked {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", e.ID, e.Enqueued.Local().Format(time.DateTime), e.Attempts, bridge.Describe(e), e.LastError)
		}
		return w.Flush()

	case "retry":
		var ids []string
		switch {
		case all:
			parked, err := st.ParkedEvents(ctx, parkedListLimit)
			if err != nil {
				return err
			}
			for _, e := range parked {
				ids = append(ids, e.ID)
			}
		case len(args) == 2:
			ids = []string{args[1]}
		default:
			return errors.New("usage: events retry ID | events retry -all")
		}
		for _, id := range ids {
			ok, err := bridge.Requeue(ctx, st, id, time.Now())
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no event %s", id)
			}
		}
		fmt.Printf("Requeued %d event(s). A running bridge picks them up within a few seconds.\n", len(ids))
		return nil

	case "drop":
		if len(args) != 2 {
			return errors.New("usage: events drop ID")
		}
		e, err := st.GetEvent(ctx, args[1])
		if err != nil {
			return err
		}
		if e == nil {
			return fmt.Errorf("no event %s", args[1])
		}
		if e.Status != store.EventParked {
			return fmt.Errorf("event %s is %s, not parked; only parked events can be dropped", e.ID, e.Status)
		}
		if err := st.DeleteEvent(ctx, e.ID); err != nil {
			return err
		}
		fmt.Printf("Dropped %s (%s).\n", e.ID, bridge.Describe(*e))
		return nil
	}
	return fmt.Errorf("unknown events command %q", args[0])
}
