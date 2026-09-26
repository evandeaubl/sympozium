package channel

import (
	"testing"
)

func TestDetectFormatEmpty(t *testing.T) {
	result := DetectFormat("")
	if result.Format != "plain" {
		t.Errorf("empty text: expected 'plain', got '%s'", result.Format)
	}
	if result.Confidence != 0.0 {
		t.Errorf("empty text: expected confidence 0.0, got %f", result.Confidence)
	}
}

func TestDetectFormatWhitespaceOnly(t *testing.T) {
	result := DetectFormat("   \n\t  \n  ")
	if result.Format != "plain" {
		t.Errorf("whitespace text: expected 'plain', got '%s'", result.Format)
	}
}

func TestDetectFormatPlainText(t *testing.T) {
	text := "Hello world, this is a simple plain text message with no formatting at all."
	result := DetectFormat(text)
	if result.Format != "plain" {
		t.Errorf("plain text: expected 'plain', got '%s' (confidence: %f)", result.Format, result.Confidence)
	}
}

func TestDetectFormatMarkdown(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"heading", "# This is a heading"},
		{"bold", "This has **bold** text"},
		{"italic", "This has *italic* text"},
		{"link", "Check out [example](https://example.com)"},
		{"code block", "```\ncode here\n```"},
		{"inline code", "Here is some `inline code`"},
		{"list", "- item 1\n- item 2\n- item 3"},
		{"ordered list", "1. First\n2. Second"},
		{"blockquote", "> This is a quote"},
		{"table", "| Col1 | Col2 |\n|------|------|\n| a    | b    |"},
		{"strikethrough", "This is ~~crossed out~~"},
		{"horizontal rule", "---\n\nText after rule"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectFormat(tt.text)
			if result.Format != "markdown" {
				t.Errorf("expected 'markdown', got '%s' (confidence: %f) for: %q", result.Format, result.Confidence, tt.text)
			}
			if result.Confidence < DefaultFormatThreshold {
				t.Errorf("confidence %f below threshold %f", result.Confidence, DefaultFormatThreshold)
			}
		})
	}
}

func TestDetectFormatHTML(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"bold tag", "This has <strong>bold</strong> text"},
		{"paragraph", "<p>This is a paragraph.</p>"},
		{"link", "Visit <a href=\"https://example.com\">example</a>"},
		{"self-closing", "Line break<br/>"},
		{"entities", "Copyright &copy; 2024"},
		{"div", "<div class=\"container\">Content</div>"},
		{"header", "<h1>Title</h1>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectFormat(tt.text)
			if result.Format != "html" {
				t.Errorf("expected 'html', got '%s' (confidence: %f) for: %q", result.Format, result.Confidence, tt.text)
			}
			if result.Confidence < DefaultFormatThreshold {
				t.Errorf("confidence %f below threshold %f", result.Confidence, DefaultFormatThreshold)
			}
		})
	}
}

func TestDetectFormatHTMLBeatsMarkdown(t *testing.T) {
	text := "<p>**bold** and <em>italic</em></p>"
	result := DetectFormat(text)
	if result.Format != "html" {
		t.Errorf("expected 'html' to win over markdown, got '%s'", result.Format)
	}
}

func TestDetectFormatMarkdownBeatsHTML(t *testing.T) {
	text := "# Heading\n\n**bold** and *italic* with a [link](url)\n\n- item 1\n- item 2"
	result := DetectFormat(text)
	if result.Format != "markdown" {
		t.Errorf("expected 'markdown' to win over html, got '%s'", result.Format)
	}
}

func TestDetectFormatMultilineMarkdown(t *testing.T) {
	text := `# Title

This is a paragraph with **bold** and *italic* text.

Here is a code example:

` + "```go\nfmt.Println(\"hello\")\n```" + `

And a list:

- Item one
- Item two
- Item three

> A blockquote here

[Link text](https://example.com)

| Col1 | Col2 |
|------|------|
| A    | B    |
`

	result := DetectFormat(text)
	if result.Format != "markdown" {
		t.Errorf("expected 'markdown', got '%s' (confidence: %f)", result.Format, result.Confidence)
	}
}

func TestDetectFormatComplexHTML(t *testing.T) {
	text := `<div class="content">
  <h1>Title</h1>
  <p>Paragraph with <strong>bold</strong> and <em>italic</em>.</p>
  <ul>
    <li>Item 1</li>
    <li>Item 2</li>
  </ul>
  <a href="https://example.com">Link</a>
  <br/>
</div>`

	result := DetectFormat(text)
	if result.Format != "html" {
		t.Errorf("expected 'html', got '%s' (confidence: %f)", result.Format, result.Confidence)
	}
}

func TestDetectFormatWithThreshold(t *testing.T) {
	text := "Just `one code span`"
	result := DetectFormat(text)
	if result.Format != "markdown" {
		t.Errorf("expected 'markdown' with default threshold, got '%s' (confidence: %f)", result.Format, result.Confidence)
	}

	result = DetectFormatWithThreshold(text, 0.05)
	if result.Format != "markdown" {
		t.Errorf("expected 'markdown' with low threshold, got '%s'", result.Format)
	}
}

func TestDetectFormatEdgeCases(t *testing.T) {
	// Text that looks like markdown but isn't (e.g., # in a non-heading context)
	text := "Temperature is 99# today"
	result := DetectFormat(text)
	if result.Format != "plain" {
		t.Errorf("expected 'plain' for '# in text', got '%s'", result.Format)
	}

	// Text with asterisk that isn't formatting (math)
	text = "Multiply: 5 * 3 = 15"
	result = DetectFormat(text)
	if result.Format != "plain" {
		t.Errorf("expected 'plain' for mathematical asterisk, got '%s'", result.Format)
	}
}

func TestScoreMarkdown(t *testing.T) {
	tests := []struct {
		text      string
		wantScore float64
	}{
		{"# Heading", 0.5},
		{"**bold**", 0.3},
		{"`code`", 0.2},
		{"", 0.0},
	}

	for _, tt := range tests {
		score := scoreMarkdown(tt.text)
		if score != tt.wantScore {
			t.Errorf("scoreMarkdown(%q) = %f, want %f", tt.text, score, tt.wantScore)
		}
	}
}

func TestScoreHTML(t *testing.T) {
	tests := []struct {
		text      string
		wantScore float64
	}{
		{"<p>", 0.4},
		{"&amp;", 0.3},
		{"</div>", 0.4},
		{"<br/>", 0.4},
		{"", 0.0},
	}

	for _, tt := range tests {
		score := scoreHTML(tt.text)
		if score != tt.wantScore {
			t.Errorf("scoreHTML(%q) = %f, want %f", tt.text, score, tt.wantScore)
		}
	}
}

func TestDetectFormatReturnsNonZeroConfidenceForFormatted(t *testing.T) {
	mdText := "# Hello\n\n**World**"
	result := DetectFormat(mdText)
	if result.Format == "plain" {
		t.Errorf("expected non-plain format, got 'plain'")
	}
	if result.Confidence <= 0.0 {
		t.Errorf("expected positive confidence, got %f", result.Confidence)
	}
}

func TestDetectFormatTie(t *testing.T) {
	// When mdScore and htmlScore are equal, markdown wins (as it's checked first)
	text := "<p># heading</p>"
	result := DetectFormat(text)
	// mdScore = 0.5 (heading), htmlScore = 0.4 (opening tag) + 0.4 (closing tag) = 0.8
	if result.Format != "html" {
		t.Errorf("expected 'html' to win, got '%s'", result.Format)
	}
}
