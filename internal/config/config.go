// Package config loads the YAML configuration file.
//
// Any string value may reference environment variables as ${NAME} or
// ${NAME:-default}, so secrets can stay out of the file.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Pairing links one Slack channel with one Webex space.
type Pairing struct {
	Name         string `yaml:"name"`
	SlackChannel string `yaml:"slack_channel"`
	WebexRoom    string `yaml:"webex_room"`
}

type Storage struct {
	// Driver is "sqlite" or "postgres".
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type Slack struct {
	// BotToken (xoxb-) is used for Web API calls.
	BotToken string `yaml:"bot_token"`
	// AppToken (xapp-) with connections:write is used for Socket Mode.
	AppToken string `yaml:"app_token"`
}

type Webex struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	RefreshToken string `yaml:"refresh_token"`
	// AccessToken is optional; one is obtained from the refresh token.
	AccessToken string `yaml:"access_token"`
	// RedirectURI and Scopes are used by the "webex-login" command.
	RedirectURI string `yaml:"redirect_uri"`
	Scopes      string `yaml:"scopes"`

	// Websocket enables the real-time device websocket. When false, or while
	// the websocket is down, paired spaces are polled instead.
	Websocket    *bool         `yaml:"websocket"`
	DeviceURL    string        `yaml:"device_url"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

// WebsocketEnabled reports whether the websocket should be used.
func (w Webex) WebsocketEnabled() bool { return w.Websocket == nil || *w.Websocket }

type Display struct {
	// SlackUsernameSuffix is appended to a Webex author's name in Slack.
	SlackUsernameSuffix string `yaml:"slack_username_suffix"`
	// WebexNameSuffix is appended to a Slack author's name in Webex.
	WebexNameSuffix string `yaml:"webex_name_suffix"`
}

type Sync struct {
	BotMessages  bool  `yaml:"bot_messages"`
	Files        bool  `yaml:"files"`
	MaxFileBytes int64 `yaml:"max_file_bytes"`
	Mentions     bool  `yaml:"mentions"`
	Reactions    bool  `yaml:"reactions"`
}

type Config struct {
	Storage  Storage   `yaml:"storage"`
	Slack    Slack     `yaml:"slack"`
	Webex    Webex     `yaml:"webex"`
	Display  Display   `yaml:"display"`
	Sync     Sync      `yaml:"sync"`
	Pairings []Pairing `yaml:"pairings"`
}

const (
	DefaultDeviceURL    = "https://wdm-a.wbx2.com/wdm/api/v1/devices"
	DefaultPollInterval = 10 * time.Second
	DefaultRedirectURI  = "http://localhost:8765/callback"
	DefaultScopes       = "spark:all spark:kms"
)

func defaults() Config {
	return Config{
		Storage: Storage{Driver: "sqlite", DSN: "slack-webex-sync.db"},
		Webex: Webex{
			DeviceURL:    DefaultDeviceURL,
			PollInterval: DefaultPollInterval,
			RedirectURI:  DefaultRedirectURI,
			Scopes:       DefaultScopes,
		},
		Display: Display{SlackUsernameSuffix: " (Webex)"},
		Sync: Sync{
			BotMessages:  true,
			Files:        true,
			MaxFileBytes: 50 << 20,
			Mentions:     true,
			Reactions:    true,
		},
	}
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv substitutes environment variables in every scalar value of the
// parsed document, so values containing YAML syntax cannot break parsing.
func expandEnv(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var missing []string
		node.Value = envPattern.ReplaceAllStringFunc(node.Value, func(match string) string {
			groups := envPattern.FindStringSubmatchIndex(match)
			name := match[groups[2]:groups[3]]
			if value, ok := os.LookupEnv(name); ok {
				return value
			}
			if groups[4] >= 0 { // "${NAME:-default}"
				return match[groups[4]:groups[5]]
			}
			missing = append(missing, name)
			return ""
		})
		if len(missing) > 0 {
			return fmt.Errorf("line %d: environment variables not set: %v", node.Line, missing)
		}
	}
	for _, child := range node.Content {
		if err := expandEnv(child); err != nil {
			return err
		}
	}
	return nil
}

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse reads and validates configuration from YAML bytes.
func Parse(raw []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := expandEnv(&doc); err != nil {
		return nil, err
	}
	cfg := defaults()
	if len(doc.Content) > 0 {
		if err := doc.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	var errs []error
	switch c.Storage.Driver {
	case "sqlite", "postgres":
	default:
		errs = append(errs, fmt.Errorf("storage.driver must be sqlite or postgres, got %q", c.Storage.Driver))
	}
	if c.Storage.DSN == "" {
		errs = append(errs, errors.New("storage.dsn is required"))
	}
	if c.Slack.BotToken == "" {
		errs = append(errs, errors.New("slack.bot_token is required"))
	}
	if c.Slack.AppToken == "" {
		errs = append(errs, errors.New("slack.app_token is required for Socket Mode"))
	}
	if c.Webex.ClientID == "" || c.Webex.ClientSecret == "" {
		errs = append(errs, errors.New("webex.client_id and webex.client_secret are required"))
	}
	if c.Webex.PollInterval <= 0 {
		errs = append(errs, errors.New("webex.poll_interval must be positive"))
	}
	if len(c.Pairings) == 0 {
		errs = append(errs, errors.New("at least one pairing is required"))
	}

	names := map[string]bool{}
	channels := map[string]bool{}
	rooms := map[string]bool{}
	for i := range c.Pairings {
		p := &c.Pairings[i]
		if p.Name == "" {
			p.Name = fmt.Sprintf("pairing-%d", i)
		}
		if p.SlackChannel == "" || p.WebexRoom == "" {
			errs = append(errs, fmt.Errorf("pairing %q needs slack_channel and webex_room", p.Name))
		}
		if names[p.Name] {
			errs = append(errs, fmt.Errorf("duplicate pairing name %q", p.Name))
		}
		if channels[p.SlackChannel] {
			errs = append(errs, fmt.Errorf("slack channel %s is in more than one pairing", p.SlackChannel))
		}
		if rooms[p.WebexRoom] {
			errs = append(errs, fmt.Errorf("webex room %s is in more than one pairing", p.WebexRoom))
		}
		names[p.Name], channels[p.SlackChannel], rooms[p.WebexRoom] = true, true, true
	}
	return errors.Join(errs...)
}
