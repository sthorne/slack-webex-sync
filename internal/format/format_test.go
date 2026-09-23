package format

import (
	"reflect"
	"testing"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

func TestSlackToWebex(t *testing.T) {
	users := map[string]model.Person{
		"U1": {ID: "U1", DisplayName: "Ada", Email: "ada@example.com"},
		"U2": {ID: "U2", DisplayName: "Bob"},
	}
	tests := []struct {
		name     string
		in       string
		mentions bool
		want     string
	}{
		{"bold", "this is *important*", true, "this is **important**"},
		{"italic unchanged", "this is _subtle_", true, "this is _subtle_"},
		{"strike", "~gone~ now", true, "~~gone~~ now"},
		{"not emphasis", "2 * 3 * 4", true, "2 * 3 * 4"},
		{"mid-word star", "a*b*c", true, "a*b*c"},
		{"link with label", "see <https://example.com/a?b=1&amp;c=2|the docs>", true, "see [the docs](https://example.com/a?b=1&c=2)"},
		{"bare link", "<https://example.com>", true, "https://example.com"},
		{"mailto", "<mailto:a@b.com|a@b.com>", true, "[a@b.com](mailto:a@b.com)"},
		{"mention with email", "hi <@U1>", true, "hi <@personEmail:ada@example.com|Ada>"},
		{"mention plain when disabled", "hi <@U1>", false, "hi @Ada"},
		{"mention without email", "hi <@U2>", true, "hi @Bob"},
		{"unknown user", "hi <@U9|zed>", true, "hi @zed"},
		{"channel", "see <#C1|general>", true, "see #general"},
		{"here", "<!here> standup", true, "<@all> standup"},
		{"here plain", "<!here> standup", false, "@here standup"},
		{"subteam", "<!subteam^S1|@oncall> help", true, "@oncall help"},
		{"entities", "a &lt; b &amp;&amp; c &gt; d", true, "a < b && c > d"},
		{"code untouched", "run `*not bold*` and *bold*", true, "run `*not bold*` and **bold**"},
		{"code block", "```\n*x* &lt;\n```", true, "```\n*x* <\n```"},
		{"bold link", "*<https://x.io|x>*", true, "**[x](https://x.io)**"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SlackToWebex(tt.in, users, tt.mentions); got != tt.want {
				t.Errorf("SlackToWebex(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestWebexToSlack(t *testing.T) {
	slackIDs := map[string]string{"Ada@Example.com": "U1"}
	emails := map[string]string{"P2": "ada@example.com"}
	tests := []struct {
		name     string
		in       string
		mentions bool
		want     string
	}{
		{"bold", "this is **important**", true, "this is *important*"},
		{"bold underscores", "this is __important__", true, "this is *important*"},
		{"italic star", "this is *subtle*", true, "this is _subtle_"},
		{"italic underscore", "this is _subtle_", true, "this is _subtle_"},
		{"bold and italic", "**a** and *b*", true, "*a* and _b_"},
		{"strike", "~~gone~~", true, "~gone~"},
		{"link", "see [the docs](https://example.com)", true, "see <https://example.com|the docs>"},
		{"escape", "a < b & c > d", true, "a &lt; b &amp; c &gt; d"},
		{"heading", "# Title\nbody", true, "*Title*\nbody"},
		{"email mention", "hi <@personEmail:ada@example.com|Ada>", true, "hi <@U1>"},
		{"person id mention", "hi <@personId:P2|Ada>", true, "hi <@U1>"},
		{"unmatched mention", "hi <@personEmail:x@y.com|Xavier>", true, "hi @Xavier"},
		{"mentions disabled", "hi <@personEmail:ada@example.com|Ada>", false, "hi @Ada"},
		{"all", "<@all> heads up", true, "<!channel> heads up"},
		{"code untouched", "`**x** <y>` and **z**", true, "`**x** &lt;y&gt;` and *z*"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WebexToSlack(tt.in, slackIDs, emails, tt.mentions); got != tt.want {
				t.Errorf("WebexToSlack(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMentionTargets(t *testing.T) {
	if got := SlackMentionIDs("<@U1> <@W2|x> <@U1>"); !reflect.DeepEqual(got, []string{"U1", "W2"}) {
		t.Errorf("SlackMentionIDs = %v", got)
	}
	emails, ids := WebexMentionTargets("<@personEmail:a@b.c|A> <@personId:P1|B> <@all>")
	if !reflect.DeepEqual(emails, []string{"a@b.c"}) || !reflect.DeepEqual(ids, []string{"P1"}) {
		t.Errorf("WebexMentionTargets = %v, %v", emails, ids)
	}
}

func TestWebexMessageMarkdownGraftsSparkMentions(t *testing.T) {
	msg := model.WebexMessage{
		Text: "Ada can you look? cc everyone",
		HTML: `<p><spark-mention data-object-type="person" data-object-id="P1">Ada</spark-mention> can you look? cc <spark-mention data-object-type="groupMention" data-group-type="all">everyone</spark-mention></p>`,
	}
	want := "<@personId:P1|Ada> can you look? cc <@all>"
	if got := WebexMessageMarkdown(msg); got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	plain := model.WebexMessage{Markdown: "**hi**", Text: "hi"}
	if got := WebexMessageMarkdown(plain); got != "**hi**" {
		t.Errorf("got %q", got)
	}
}

func TestReactions(t *testing.T) {
	if got := SlackReactionForWebex("thumbsup"); got != "+1" {
		t.Errorf("thumbsup -> %q", got)
	}
	if got := SlackReactionForWebex("rocket"); got != "rocket" {
		t.Errorf("rocket -> %q", got)
	}
	if got := SlackReactionText("+1::skin-tone-3"); got != "👍" {
		t.Errorf("+1 -> %q", got)
	}
	if got := SlackReactionText("partyparrot"); got != ":partyparrot:" {
		t.Errorf("custom -> %q", got)
	}
}
