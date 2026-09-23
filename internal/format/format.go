// Package format translates message text between Slack mrkdwn and Webex
// markdown.
//
// Both directions follow the same steps. Code spans and blocks are left
// alone. Platform tokens (mentions, links) become placeholders so the
// emphasis rewrites cannot damage them. The placeholders are restored at the
// end.
//
// Mentions need lookups (Slack user -> email -> Webex, and back), so callers
// first list what needs resolving (SlackMentionIDs, WebexMentionTargets) and
// then pass the resolved values in.
package format

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

var (
	codePattern = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`")
	placeholder = regexp.MustCompile("\x00(\\d+)\x00")

	// <@U123>, <@U123|name>, <#C123|name>, <!here>, <!subteam^S1|@team>,
	// <!date^...|fallback>, <https://x|label>, <https://x>, <mailto:a@b|a@b>
	slackToken  = regexp.MustCompile(`<([@#!]?)([^<>|]*)(?:\|([^<>]*))?>`)
	slackUserID = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^<>]*)?>`)

	webexMention = regexp.MustCompile(`(?i)<@(personEmail|personId):([^|>]+)(?:\|([^>]*))?>|<@(all|here)>`)
	sparkMention = regexp.MustCompile(`(?s)<spark-mention([^>]*)>(.*?)</spark-mention>`)
	sparkAttr    = regexp.MustCompile(`data-object-(type|id)="([^"]*)"`)
	mdLink       = regexp.MustCompile(`(!?)\[([^\]]+)\]\(([^)\s]+)\)`)
	mdHeading    = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*#*[ \t]*$`)
	htmlTag      = regexp.MustCompile(`<[^>]+>`)
)

type holders []string

func (h *holders) add(value string) string {
	*h = append(*h, value)
	return fmt.Sprintf("\x00%d\x00", len(*h)-1)
}

func (h holders) restore(s string) string {
	// Values can themselves contain placeholders (a link inside bold).
	for placeholder.MatchString(s) {
		s = placeholder.ReplaceAllStringFunc(s, func(m string) string {
			i, _ := strconv.Atoi(m[1 : len(m)-1])
			return h[i]
		})
	}
	return s
}

// mapOutsideCode applies prose to text outside code and code to code.
func mapOutsideCode(s string, prose, code func(string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range codePattern.FindAllStringIndex(s, -1) {
		b.WriteString(prose(s[last:loc[0]]))
		b.WriteString(code(s[loc[0]:loc[1]]))
		last = loc[1]
	}
	b.WriteString(prose(s[last:]))
	return b.String()
}

func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func runeBefore(s string, i int) rune {
	if i <= 0 {
		return ' '
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r
}

func runeAt(s string, i int) rune {
	if i >= len(s) {
		return ' '
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r
}

// replaceEmphasis rewrites spans wrapped in delim (such as "*", "**" or
// "~~") that follow the usual emphasis rules: the opening delimiter is not
// preceded by a word character, the span does not start or end with
// whitespace, the closing delimiter is not followed by a word character,
// and the span stays on one line.
func replaceEmphasis(s, delim string, wrap func(inner string) string) string {
	marker := rune(delim[0])
	var b strings.Builder
	i := 0
	for i < len(s) {
		if !strings.HasPrefix(s[i:], delim) {
			b.WriteByte(s[i])
			i++
			continue
		}
		prev := runeBefore(s, i)
		start := i + len(delim)
		first := runeAt(s, start)
		if isWord(prev) || prev == marker || unicode.IsSpace(first) || first == marker || start >= len(s) {
			b.WriteString(delim)
			i = start
			continue
		}
		end := findClosing(s, start, delim)
		if end < 0 {
			b.WriteString(delim)
			i = start
			continue
		}
		b.WriteString(wrap(s[start:end]))
		i = end + len(delim)
	}
	return b.String()
}

func findClosing(s string, from int, delim string) int {
	marker := rune(delim[0])
	for j := from; j < len(s); j++ {
		if s[j] == '\n' {
			return -1
		}
		if !strings.HasPrefix(s[j:], delim) || j == from {
			continue
		}
		last := runeBefore(s, j)
		next := runeAt(s, j+len(delim))
		if unicode.IsSpace(last) || isWord(next) || next == marker {
			continue
		}
		if len(delim) == 1 && last == marker {
			continue
		}
		return j
	}
	return -1
}

// ---------------------------------------------------------------------------
// Slack -> Webex
// ---------------------------------------------------------------------------

// SlackMentionIDs returns the Slack user ids mentioned in text.
func SlackMentionIDs(text string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, m := range slackUserID.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
		}
	}
	return ids
}

// SlackToWebex converts Slack mrkdwn to Webex markdown. users maps Slack user
// ids to people. When mentions is true and a person has an email, the
// mention becomes a real Webex mention; otherwise it becomes "@Name".
func SlackToWebex(text string, users map[string]model.Person, mentions bool) string {
	prose := func(seg string) string {
		var h holders
		seg = slackToken.ReplaceAllStringFunc(seg, func(m string) string {
			g := slackToken.FindStringSubmatch(m)
			sigil, target, label := g[1], g[2], g[3]
			switch sigil {
			case "@":
				person, ok := users[target]
				name := label
				if ok {
					name = person.DisplayName
				}
				if name == "" {
					name = target
				}
				if mentions && ok && person.Email != "" {
					return h.add(fmt.Sprintf("<@personEmail:%s|%s>", person.Email, name))
				}
				return h.add("@" + name)
			case "#":
				if label == "" {
					label = target
				}
				return h.add("#" + label)
			case "!":
				keyword, _, _ := strings.Cut(target, "^")
				switch keyword {
				case "here", "channel", "everyone":
					if mentions {
						return h.add("<@all>")
					}
					return h.add("@" + keyword)
				}
				if label != "" {
					return h.add(html.UnescapeString(label))
				}
				return h.add("@" + keyword)
			}
			url := html.UnescapeString(target)
			if label != "" {
				return h.add(fmt.Sprintf("[%s](%s)", html.UnescapeString(label), url))
			}
			return h.add(strings.TrimPrefix(url, "mailto:"))
		})
		seg = html.UnescapeString(seg)
		seg = replaceEmphasis(seg, "*", func(in string) string { return h.add("**" + in + "**") })
		seg = replaceEmphasis(seg, "~", func(in string) string { return h.add("~~" + in + "~~") })
		return h.restore(seg)
	}
	return mapOutsideCode(text, prose, html.UnescapeString)
}

// ---------------------------------------------------------------------------
// Webex -> Slack
// ---------------------------------------------------------------------------

// WebexMentionTargets returns the emails and person ids mentioned in Webex
// markdown.
func WebexMentionTargets(markdown string) (emails, personIDs []string) {
	for _, m := range webexMention.FindAllStringSubmatch(markdown, -1) {
		switch strings.ToLower(m[1]) {
		case "personemail":
			emails = append(emails, m[2])
		case "personid":
			personIDs = append(personIDs, m[2])
		}
	}
	return emails, personIDs
}

// WebexMessageMarkdown picks the best source text from a Webex message.
// Mentions made in Webex clients arrive only in the html field as
// <spark-mention> tags. When the markdown has no mention syntax, those
// mentions are grafted onto the plain text as <@personId:...> tokens.
func WebexMessageMarkdown(msg model.WebexMessage) string {
	body := msg.Markdown
	if body == "" {
		body = msg.Text
	}
	if msg.HTML == "" || webexMention.MatchString(body) || !sparkMention.MatchString(msg.HTML) {
		return body
	}
	for _, m := range sparkMention.FindAllStringSubmatch(msg.HTML, -1) {
		attrs := map[string]string{}
		for _, a := range sparkAttr.FindAllStringSubmatch(m[1], -1) {
			attrs[a[1]] = a[2]
		}
		name := html.UnescapeString(htmlTag.ReplaceAllString(m[2], ""))
		if name == "" {
			continue
		}
		var token string
		switch {
		case attrs["type"] == "groupMention":
			token = "<@all>"
		case attrs["type"] == "person" && attrs["id"] != "":
			token = fmt.Sprintf("<@personId:%s|%s>", attrs["id"], name)
		default:
			continue
		}
		body = strings.Replace(body, name, token, 1)
	}
	return body
}

func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// WebexToSlack converts Webex markdown to Slack mrkdwn. slackIDsByEmail
// turns a mentioned email into a Slack user id (so the mention notifies
// them). emailsByPersonID resolves <@personId:...> mentions to emails.
func WebexToSlack(markdown string, slackIDsByEmail, emailsByPersonID map[string]string, mentions bool) string {
	byEmail := make(map[string]string, len(slackIDsByEmail))
	for k, v := range slackIDsByEmail {
		byEmail[strings.ToLower(k)] = v
	}
	prose := func(seg string) string {
		var h holders
		seg = webexMention.ReplaceAllStringFunc(seg, func(m string) string {
			g := webexMention.FindStringSubmatch(m)
			kind, value, label, group := strings.ToLower(g[1]), g[2], g[3], g[4]
			if group != "" {
				if mentions {
					return h.add("<!channel>")
				}
				return h.add("@all")
			}
			email := value
			if kind == "personid" {
				email = emailsByPersonID[value]
			}
			if id := byEmail[strings.ToLower(email)]; mentions && id != "" {
				return h.add("<@" + id + ">")
			}
			name := label
			if name == "" {
				name = email
			}
			if name == "" {
				name = "someone"
			}
			return h.add(slackEscape("@" + name))
		})
		seg = sparkMention.ReplaceAllString(seg, "@$2")
		seg = mdLink.ReplaceAllStringFunc(seg, func(m string) string {
			// Images become plain links too; Slack mrkdwn cannot embed them.
			g := mdLink.FindStringSubmatch(m)
			return h.add(fmt.Sprintf("<%s|%s>", g[3], slackEscape(g[2])))
		})
		seg = slackEscape(seg)
		seg = mdHeading.ReplaceAllStringFunc(seg, func(m string) string {
			return h.add("*" + mdHeading.FindStringSubmatch(m)[1] + "*")
		})
		// Italic first: afterwards any remaining star pairs are bold markers.
		seg = replaceEmphasis(seg, "*", func(in string) string { return h.add("_" + in + "_") })
		seg = replaceEmphasis(seg, "**", func(in string) string { return h.add("*" + in + "*") })
		seg = replaceEmphasis(seg, "__", func(in string) string { return h.add("*" + in + "*") })
		seg = replaceEmphasis(seg, "~~", func(in string) string { return h.add("~" + in + "~") })
		return h.restore(seg)
	}
	return mapOutsideCode(markdown, prose, slackEscape)
}
