package config

import (
	"strings"
	"testing"
	"time"
)

const valid = `
storage:
  driver: sqlite
  dsn: ${SWS_TEST_DB:-bridge.db}
slack:
  bot_token: ${SWS_TEST_BOT}
  app_token: "xapp-1"
webex:
  client_id: id
  client_secret: "${SWS_TEST_SECRET}"
  poll_interval: 30s
  websocket: false
sync:
  reactions: false
pairings:
  - name: eng
    slack_channel: C1
    webex_room: R1
  - slack_channel: C2
    webex_room: R2
`

func TestParse(t *testing.T) {
	t.Setenv("SWS_TEST_BOT", "xoxb-1")
	t.Setenv("SWS_TEST_SECRET", "has: yaml # chars")
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.DSN != "bridge.db" {
		t.Errorf("default not applied: %q", cfg.Storage.DSN)
	}
	if cfg.Slack.BotToken != "xoxb-1" || cfg.Webex.ClientSecret != "has: yaml # chars" {
		t.Errorf("env not expanded: %+v %+v", cfg.Slack, cfg.Webex)
	}
	if cfg.Webex.PollInterval != 30*time.Second || cfg.Webex.WebsocketEnabled() {
		t.Errorf("webex settings: %+v", cfg.Webex)
	}
	if cfg.Webex.DeviceURL != DefaultDeviceURL {
		t.Errorf("device url default: %q", cfg.Webex.DeviceURL)
	}
	if !cfg.Sync.BotMessages || !cfg.Sync.Files || cfg.Sync.Reactions {
		t.Errorf("sync settings: %+v", cfg.Sync)
	}
	if cfg.Pairings[1].Name != "pairing-1" {
		t.Errorf("unnamed pairing got %q", cfg.Pairings[1].Name)
	}
}

func TestParseErrors(t *testing.T) {
	t.Setenv("SWS_TEST_BOT", "xoxb-1")
	t.Setenv("SWS_TEST_SECRET", "s")
	tests := map[string]struct{ from, to, want string }{
		"missing env":       {"${SWS_TEST_BOT}", "${SWS_TEST_UNSET_VAR}", "SWS_TEST_UNSET_VAR"},
		"bad driver":        {"driver: sqlite", "driver: mysql", "storage.driver"},
		"missing app token": {`app_token: "xapp-1"`, "", "app_token"},
		"duplicate channel": {"slack_channel: C2", "slack_channel: C1", "more than one pairing"},
		"duplicate room":    {"webex_room: R2", "webex_room: R1", "more than one pairing"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(valid, tt.from, tt.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestExampleConfigParses(t *testing.T) {
	for _, name := range []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "WEBEX_CLIENT_ID", "WEBEX_CLIENT_SECRET"} {
		t.Setenv(name, "x")
	}
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Pairings) != 2 || cfg.Webex.PollInterval != 10*time.Second || !cfg.Webex.WebsocketEnabled() {
		t.Errorf("unexpected example config: %+v", cfg)
	}
}
