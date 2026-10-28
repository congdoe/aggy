package borrow

import (
	"fmt"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
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
}

// kindGuide is the report shape asked of each kind.
var kindGuide = map[adapter.Kind]string{
	adapter.Research: "Search the web. Cite every source with its publish date, say what the findings are as of, and list open questions.",
	adapter.News:     "Search for the latest news. Cite every source with its publish date, say what the news is as of, and list open questions.",
	adapter.Image:    "Generate the image the task describes. Say how many images you made and describe each in one line.",
	adapter.Video:    "Generate the video the task describes. Say what you made in one line.",
	adapter.Review:   "Review the code read-only. Do not change any file. List findings by severity with file:line and a concrete failure scenario.",
}

// Text renders the prompt as Markdown.
func (p Prompt) Text() string {
	var b strings.Builder
	b.WriteString("You are a headless helper for another coding agent. Do the one task below and reply with a short Markdown report.\n")
	b.WriteString("Start the report with a one-line summary. Do not start other agents or CLIs.\n\n")
	fmt.Fprintf(&b, "Kind: %s\nToday's date: %s\n\n", p.Kind, p.Today.Format("2006-01-02"))
	b.WriteString("## Task\n\n")
	b.WriteString(strings.TrimSpace(p.Task))
	b.WriteString("\n")
	if len(p.Paths) > 0 {
		b.WriteString("\n## Paths\n\n")
		for _, x := range p.Paths {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	if len(p.Constraints) > 0 {
		b.WriteString("\n## Constraints\n\n")
		for _, x := range p.Constraints {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(x))
		}
	}
	if g := kindGuide[p.Kind]; g != "" {
		fmt.Fprintf(&b, "\n## Report\n\n%s\n", g)
	}
	return b.String()
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
