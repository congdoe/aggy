package borrow

import (
	"fmt"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/redact"
)

// Prompt is everything a borrow child is told. It holds no parent
// transcript: the child sees the task, the paths and constraints the
// caller passed, and the date, and nothing else.
type Prompt struct {
	Kind        adapter.Kind
	Task        string
	Paths       []string
	Constraints []string
	Today       time.Time
	// Material is the review input, for review only.
	Material *Material
}

// kindGuide is the report shape asked of each kind.
var kindGuide = map[adapter.Kind]string{
	adapter.Research: "Search the web. " + webReport,
	adapter.News:     "Search for the latest news. " + webReport,
	adapter.Image:    "Generate the image the task describes. Say how many images you made and describe each in one line.",
	adapter.Video:    "Generate the video the task describes. Say what you made in one line.",
	adapter.Review:   "Review the change in the Material section read-only. Do not change any file. List findings by severity with file:line and a concrete failure scenario.",
}

// webReport is the report shape of research and news: aggy shows the
// parent the first lines, so the summary leads.
const webReport = "Use these sections, in this order: `## Summary` (at most 8 lines, the answer first), " +
	"`## Findings`, `## Sources` (one line each: title, URL, and publish date as YYYY-MM-DD, or \"date unknown\"), " +
	"`## As of` (the newest date your sources cover), and `## Open questions`. Cite a source for every finding."

// Text renders the prompt as Markdown. Every part the caller passed (the
// task, paths, constraints, and review material) is redacted on its own
// (redact.Lines) before it goes in, and the prompt says how many lines
// were removed. Only the prompt file the child reads is built here.
func (p Prompt) Text() string {
	removed := 0
	clean := func(s string) string {
		s, n := redact.Lines(s)
		removed += n
		return s
	}
	var b strings.Builder
	b.WriteString("You are a headless helper for another coding agent. Do the one task below and reply with a short Markdown report.\n")
	b.WriteString("Start the report with a one-line summary. Do not start other agents or CLIs.\n\n")
	fmt.Fprintf(&b, "Kind: %s\nToday's date: %s\n\n", p.Kind, p.Today.Format("2006-01-02"))
	b.WriteString("## Task\n\n")
	b.WriteString(strings.TrimSpace(clean(p.Task)))
	b.WriteString("\n")
	list := func(title string, xs []string) {
		var keep []string
		for _, x := range xs {
			if x = strings.TrimSpace(clean(x)); x != "" {
				keep = append(keep, x)
			}
		}
		if len(keep) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", title)
		for _, x := range keep {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	list("Paths", p.Paths)
	list("Constraints", p.Constraints)
	if g := kindGuide[p.Kind]; g != "" {
		fmt.Fprintf(&b, "\n## Report\n\n%s\n", g)
	}
	if p.Material != nil {
		b.WriteString("\n## Material\n\n")
		text, n := p.Material.Text(MaxMaterial)
		removed += n
		b.WriteString(text)
	}
	if removed > 0 {
		fmt.Fprintf(&b, "\n%s\n", removedNote(removed, "this prompt"))
	}
	return b.String()
}

// removedNote says that n secret-looking lines were removed from what.
func removedNote(n int, what string) string {
	return fmt.Sprintf("%d lines that looked like secrets (keys, tokens, private keys, secret settings) were removed from %s.", n, what)
}

// Excerpt returns up to n non-empty lines of text, each cut to width
// runes, for the short report. It escapes nothing: callers print it
// through termsafe.
func Excerpt(text string, n, width int) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		if r := []rune(l); len(r) > width {
			l = string(r[:width-1]) + "…"
		}
		out = append(out, l)
		if len(out) == n {
			break
		}
	}
	return out
}
