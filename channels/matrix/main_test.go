package main

import (
	"strings"
	"sync"
	"testing"
)

func TestExtractDisplayNameFromMXID(t *testing.T) {
	tests := []struct {
		mxid string
		want string
	}{
		{"@mybot:matrix.org", "mybot"},
		{"@alice:example.com", "alice"},
		{"@bob:matrix.example.org", "bob"},
		{"unknown", "unknown"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.mxid, func(t *testing.T) {
			got := extractDisplayNameFromMXID(tt.mxid)
			if got != tt.want {
				t.Errorf("extractDisplayNameFromMXID(%q) = %q, want %q", tt.mxid, got, tt.want)
			}
		})
	}
}

func TestRedactError(t *testing.T) {
	mc := &MatrixChannel{accessToken: "s3cret_token_123"}

	in := "GET https://matrix.org/_matrix/client/v3/sync?access_token=s3cret_token_123: timeout"
	out := mc.redactError(in)

	if strings.Contains(out, "s3cret_token_123") {
		t.Errorf("redactError left the access token in the string: %q", out)
	}
	if !strings.Contains(out, "***REDACTED***") {
		t.Errorf("redactError did not insert the redaction marker: %q", out)
	}
	if !strings.Contains(out, "timeout") {
		t.Errorf("redactError removed non-secret content: %q", out)
	}
}

func TestRedactErrorEmpty(t *testing.T) {
	mc := &MatrixChannel{accessToken: ""}
	in := "some error"
	if got := mc.redactError(in); got != in {
		t.Errorf("redactError with empty token = %q, want unchanged %q", got, in)
	}
}

func TestConvertMarkdownToHTML(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "plain text",
			input: "Hello world",
			want:  "Hello world",
		},
		{
			name:  "bold and italic",
			input: "**bold** and *italic*",
			want:  "<strong>bold</strong> and <em>italic</em>",
		},
		{
			name:  "heading",
			input: "# Heading 1",
			want:  "<h1 id=\"heading-1\">Heading 1</h1>",
		},
		{
			name:  "code block",
			input: "```\ncode\n```",
			want:  "<pre><code>code\n</code></pre>",
		},
		{
			name:  "inline code",
			input: "Here is some `code`",
			want:  "Here is some <code>code</code>",
		},
		{
			name:  "link",
			input: "[example](https://example.com)",
			want:  `<a href="https://example.com">example</a>`,
		},
		{
			name:  "unordered list",
			input: "- item 1\n- item 2",
			want:  "<ul>\n<li>item 1</li>\n<li>item 2</li>\n</ul>",
		},
		{
			name:  "bold with markdown symbols",
			input: "**hello**",
			want:  "<strong>hello</strong>",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:  "multiple paragraphs",
			input: "First paragraph\n\nSecond paragraph",
			want:  "<p>First paragraph</p>\n<p>Second paragraph</p>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertMarkdownToHTML(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("convertMarkdownToHTML() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil && !strings.Contains(got, tt.want) {
				t.Errorf("convertMarkdownToHTML() = %q, want to contain %q", got, tt.want)
			}
		})
	}
}

func TestConvertMarkdownToHTMLKnownFormats(t *testing.T) {
	// Test that we can convert various markdown inputs without errors
	// for the formats the channel supports.
	knownMarkdown := []string{
		"# Heading",
		"**bold** text",
		"- item 1\n- item 2",
		"```go\nfmt.Println(\"hello\")\n```",
		"[link](https://example.com)",
		"> blockquote",
	}
	for _, md := range knownMarkdown {
		html, err := convertMarkdownToHTML(md)
		if err != nil {
			t.Errorf("convertMarkdownToHTML(%q) error: %v", md, err)
		}
		if html == "" {
			t.Errorf("convertMarkdownToHTML(%q) returned empty HTML", md)
		}
	}
}

func TestConvertMarkdownToHTMLEmpty(t *testing.T) {
	got, err := convertMarkdownToHTML("")
	if err == nil {
		t.Errorf("expected error for empty input, got %q", got)
	}
}

func TestIsMention(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		botUserID string
		want     bool
	}{
		{"full MXID mention", "Hey @mybot:matrix.org can you help?", "@mybot:matrix.org", true},
		{"full MXID mention case insensitive", "Hey @MYBOT:matrix.org help", "@mybot:matrix.org", true},
		{"bare localpart mention", "Hey @mybot can you help?", "@mybot:matrix.org", true},
		{"bare localpart at start", "@mybot ping", "@mybot:matrix.org", true},
		{"no mention", "Can anyone help?", "@mybot:matrix.org", false},
		{"empty text", "", "@mybot:matrix.org", false},
		{"empty botID", "hello", "", false},
		{"partial match not mention", "mybotify is cool", "@mybot:matrix.org", false},
		{"mention with <@> wrap", "Hey <@mybot:matrix.org> help", "@mybot:matrix.org", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMention(tt.text, tt.botUserID)
			if got != tt.want {
				t.Errorf("isMention(%q, %q) = %v, want %v", tt.text, tt.botUserID, got, tt.want)
			}
		})
	}
}

func TestTriggerAllowed(t *testing.T) {
	tests := []struct {
		name     string
		allowed  map[string]bool
		k        triggerKind
		want     bool
	}{
		{"empty allowlist allows all", map[string]bool{}, kindChannel, true},
		{"empty allowlist allows dm", map[string]bool{}, kindDM, true},
		{"empty allowlist allows mention", map[string]bool{}, kindMention, true},
		{"mention allowed", map[string]bool{"mention": true}, kindMention, true},
		{"dm allowed", map[string]bool{"dm": true}, kindDM, true},
		{"channel allowed", map[string]bool{"channel": true}, kindChannel, true},
		{"mention not allowed", map[string]bool{"dm": true}, kindMention, false},
		{"dm not allowed", map[string]bool{"channel": true}, kindDM, false},
		{"channel not allowed", map[string]bool{"mention": true}, kindChannel, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &matrixConfig{allowedTriggers: tt.allowed}
			got := c.triggerAllowed(tt.k)
			if got != tt.want {
				t.Errorf("triggerAllowed(%v) with allowed=%v = %v, want %v", tt.k, tt.allowed, got, tt.want)
			}
		})
	}
}

func TestCsvToSet(t *testing.T) {
	tests := []struct {
		name string
		input string
		want  map[string]bool
	}{
		{"single value", "mention", map[string]bool{"mention": true}},
		{"multiple values", "mention,dm,channel", map[string]bool{"mention": true, "dm": true, "channel": true}},
		{"with spaces", " mention , dm , channel ", map[string]bool{"mention": true, "dm": true, "channel": true}},
		{"empty string", "", map[string]bool{}},
		{"only commas and spaces", " , , ", map[string]bool{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := csvToSet(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("csvToSet(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for k := range tt.want {
				if !got[k] {
					t.Errorf("csvToSet(%q) missing key %q", tt.input, k)
				}
			}
		})
	}
}

func TestNextTxnID(t *testing.T) {
	var txnMu sync.Mutex
	var lastTxnID int64

	// First call should return 1.
	id1 := nextTxnID(&txnMu, &lastTxnID)
	if id1 != 1 {
		t.Errorf("first txnID = %d, want 1", id1)
	}

	// Second call should return 2.
	id2 := nextTxnID(&txnMu, &lastTxnID)
	if id2 != 2 {
		t.Errorf("second txnID = %d, want 2", id2)
	}

	// IDs should be monotonically increasing.
	for i := 0; i < 100; i++ {
		id := nextTxnID(&txnMu, &lastTxnID)
		if id <= id2 {
			t.Errorf("txnID not monotonically increasing: got %d after %d", id, id2)
		}
		lastTxnID = id
	}
}
