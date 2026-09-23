package format

import "strings"

// webexToSlackReaction maps Webex reaction names to Slack emoji names.
var webexToSlackReaction = map[string]string{
	"smiley":     "smiley",
	"sad":        "cry",
	"wow":        "open_mouth",
	"haha":       "joy",
	"celebrate":  "tada",
	"heart":      "heart",
	"thumbsup":   "+1",
	"thumbsdown": "-1",
	"prayer":     "pray",
	"fire":       "fire",
	"clap":       "clap",
}

// SlackReactionForWebex returns the Slack emoji name for a Webex reaction.
// Unknown names are passed through; Slack rejects the ones it doesn't know.
func SlackReactionForWebex(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if slack, ok := webexToSlackReaction[name]; ok {
		return slack
	}
	return name
}

// slackEmoji maps common Slack emoji names to Unicode for reaction notes.
var slackEmoji = map[string]string{
	"+1": "👍", "thumbsup": "👍", "-1": "👎", "thumbsdown": "👎",
	"heart": "❤️", "joy": "😂", "smile": "😄", "smiley": "😃", "grinning": "😀",
	"laughing": "😆", "slightly_smiling_face": "🙂", "wink": "😉", "cry": "😢",
	"sob": "😭", "open_mouth": "😮", "astonished": "😲", "thinking_face": "🤔",
	"tada": "🎉", "pray": "🙏", "fire": "🔥", "clap": "👏", "eyes": "👀",
	"white_check_mark": "✅", "heavy_check_mark": "✔️", "x": "❌", "warning": "⚠️",
	"rocket": "🚀", "100": "💯", "raised_hands": "🙌", "ok_hand": "👌", "wave": "👋",
	"muscle": "💪", "star": "⭐", "sparkles": "✨", "bulb": "💡", "question": "❓",
	"exclamation": "❗", "heavy_plus_sign": "➕", "point_up": "☝️", "see_no_evil": "🙈",
	"sweat_smile": "😅", "rolling_on_the_floor_laughing": "🤣", "heart_eyes": "😍",
	"slightly_frowning_face": "🙁", "confused": "😕", "facepalm": "🤦", "shrug": "🤷",
}

// SlackReactionText renders a Slack reaction name for display in Webex.
func SlackReactionText(name string) string {
	base, _, _ := strings.Cut(name, "::") // drop skin-tone suffixes
	if emoji, ok := slackEmoji[base]; ok {
		return emoji
	}
	return ":" + name + ":"
}
