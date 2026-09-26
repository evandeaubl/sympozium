// Package channel provides base types and interfaces for Sympozium channel implementations.
// Each channel type (Telegram, WhatsApp, Discord, Slack) runs as its own pod
// and uses this framework to connect to the event bus.
package channel

import (
	"regexp"
	"strings"
)

// DetectedFormat represents a detected text format with a confidence score.
// The score is in the range [0.0, 1.0], where higher means more likely.
type DetectedFormat struct {
	Format     string  // "markdown", "html", or "plain"
	Confidence float64 // 0.0 to 1.0
}

// DefaultFormatThreshold is the minimum confidence score required to classify
// text as a non-plain format. A single markdown pattern (0.2-0.5) clears this
// threshold, while very weak signals like a lone asterisk in prose do not.
const DefaultFormatThreshold = 0.2

// DetectFormat examines text content and determines if it contains Markdown or HTML
// formatting. It uses a heuristic scoring approach based on the frequency and
// strength of format-specific syntax patterns.
//
// Each pattern contributes a weighted score:
//   - Strong patterns (code blocks, headings): 0.5
//   - Medium patterns (bold, links, tables, blockquotes, lists): 0.3
//   - Weak patterns (italic, inline code, strikethrough): 0.2
//   - HTML tags: 0.4 each
//
// The total is capped at 1.0. If the winning format's score is below
// DefaultFormatThreshold, the result is "plain".
func DetectFormat(text string) DetectedFormat {
	return DetectFormatWithThreshold(text, DefaultFormatThreshold)
}

// DetectFormatWithThreshold works like DetectFormat but allows specifying a custom threshold.
func DetectFormatWithThreshold(text string, threshold float64) DetectedFormat {
	if strings.TrimSpace(text) == "" {
		return DetectedFormat{Format: "plain", Confidence: 0.0}
	}

	mdScore := scoreMarkdown(text)
	htmlScore := scoreHTML(text)

	if mdScore >= htmlScore {
		if mdScore >= threshold {
			return DetectedFormat{Format: "markdown", Confidence: mdScore}
		}
	} else {
		if htmlScore >= threshold {
			return DetectedFormat{Format: "html", Confidence: htmlScore}
		}
	}

	return DetectedFormat{Format: "plain", Confidence: 0.0}
}

// Weighted markdown patterns: (regex, weight)
var markdownPatterns = []struct {
	re     *regexp.Regexp
	weight float64
}{
	{regexp.MustCompile(`(?m)^#{1,6}\s+\S`), 0.5},
	{regexp.MustCompile("(?s)```[a-zA-Z]*\n.*?```|~~~[a-zA-Z]*\n.*?~~~"), 0.5},
	{regexp.MustCompile(`\*\*[^*]+\*\*|__[^_]+__`), 0.3},
	{regexp.MustCompile(`\[[^\]]+\]\([^)]+\)|\[[^\]]+\]\[[^\]]*\]`), 0.3},
	{regexp.MustCompile(`(?m)^\|.*\|$`), 0.3},
	{regexp.MustCompile(`(?m)^>\s`), 0.3},
	{regexp.MustCompile(`(?m)^[\s]*[-*+]\s+\S|^[\s]*\d+\.\s+\S`), 0.3},
	{regexp.MustCompile(`~~[^~]+~~`), 0.2},
	{regexp.MustCompile(`(^|\s|_)\*[^*]+\*(\s|$)`), 0.2},
	{regexp.MustCompile("(`[^`]+`)"), 0.2},
	{regexp.MustCompile(`(?m)^[ \t]*(---|\*\*\*|___)[ \t]*$`), 0.2},
}

// Weighted HTML patterns: (regex, weight)
var htmlPatterns = []struct {
	re      *regexp.Regexp
	weight  float64
}{
	{regexp.MustCompile(`<[a-zA-Z][a-zA-Z0-9]*(?:[\s/>])`), 0.4},
	{regexp.MustCompile(`&[a-zA-Z#0-9]+;`), 0.3},
	{regexp.MustCompile(`</[a-zA-Z][a-zA-Z0-9]*>`), 0.4},
}

// scoreMarkdown evaluates how likely the text contains Markdown formatting.
// Returns a normalized confidence score between 0.0 and 1.0.
func scoreMarkdown(text string) float64 {
	var score float64
	for _, p := range markdownPatterns {
		if p.re.MatchString(text) {
			score += p.weight
		}
	}
	if score > 1.0 {
		score = 1.0
	}
	return score
}

// scoreHTML evaluates how likely the text contains HTML formatting.
// Returns a normalized confidence score between 0.0 and 1.0.
func scoreHTML(text string) float64 {
	var score float64
	for _, p := range htmlPatterns {
		if p.re.MatchString(text) {
			score += p.weight
		}
	}
	if score > 1.0 {
		score = 1.0
	}
	return score
}
