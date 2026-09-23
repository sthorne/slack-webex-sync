// Package model holds the platform-neutral types shared by the Slack and
// Webex adapters and the bridge.
package model

// Platform identifies where a message originated.
type Platform string

const (
	Slack Platform = "slack"
	Webex Platform = "webex"
)

// Person is an author or mentioned user on either platform.
type Person struct {
	ID          string
	DisplayName string
	Email       string
	AvatarURL   string
	IsBot       bool
}

// Attachment is a file being copied between platforms.
type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

// SlackFile is a file shared in a Slack message.
type SlackFile struct {
	Name        string
	Mimetype    string
	Size        int
	DownloadURL string
}

// SlackMessage is the subset of a Slack message the bridge needs.
type SlackMessage struct {
	Channel  string
	TS       string
	ThreadTS string
	User     string
	BotID    string
	Username string // set on bot and webhook posts
	Text     string
	Files    []SlackFile
}

// SlackEventKind enumerates the Slack events the bridge reacts to.
type SlackEventKind int

const (
	SlackMessagePosted SlackEventKind = iota + 1
	SlackMessageEdited
	SlackMessageDeleted
	SlackReactionAdded
	SlackReactionRemoved
)

// SlackEvent is a normalized Slack event.
type SlackEvent struct {
	Kind    SlackEventKind
	Channel string
	// Message is set for posted and edited events.
	Message SlackMessage
	// PreviousText is the text before an edit.
	PreviousText string
	// TS is the affected message for deletions and reactions.
	TS string
	// User and Reaction are set for reaction events.
	User     string
	Reaction string
}

// WebexEventKind enumerates the Webex events the bridge reacts to.
type WebexEventKind int

const (
	WebexMessageCreated WebexEventKind = iota + 1
	WebexMessageUpdated
	// WebexDeleted means an activity was deleted. It may be a message or a
	// reaction; the bridge checks which.
	WebexDeleted
	WebexReactionAdded
)

// WebexEvent is a normalized Webex event from the websocket or the poller.
type WebexEvent struct {
	Kind    WebexEventKind
	RoomID  string
	ActorID string
	// MessageID is the REST id of the message affected (for reactions, the
	// message that was reacted to).
	MessageID string
	// ActivityID is the raw activity id for deletions and reactions.
	ActivityID string
	// Reaction is the Webex reaction name, e.g. "thumbsup".
	Reaction string
}

// WebexMessage is a message as returned by the Webex REST API.
type WebexMessage struct {
	ID              string   `json:"id"`
	RoomID          string   `json:"roomId"`
	ParentID        string   `json:"parentId"`
	PersonID        string   `json:"personId"`
	PersonEmail     string   `json:"personEmail"`
	Text            string   `json:"text"`
	Markdown        string   `json:"markdown"`
	HTML            string   `json:"html"`
	Files           []string `json:"files"`
	MentionedPeople []string `json:"mentionedPeople"`
	MentionedGroups []string `json:"mentionedGroups"`
	Created         string   `json:"created"`
	Updated         string   `json:"updated"`
}
