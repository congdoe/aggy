package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/termsafe"
)

// permissionsShow is aggy permissions show: the rules in force, where each came
// from, what each CLI gets, and what was skipped and why. It only reads.
// TODO(T6908): /aggy permissions in a session shows this too, once batch 6's
// /aggy commands exist.
func permissionsShow(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		return cmdUsage(stderr, "permissions show", "takes no arguments", permissionsUsage)
	}
	s, in, cfg, err := permissionsHere(context.Background())
	if err != nil {
		return cmdErr(stderr, "permissions show", diag.CodeConfig, err)
	}
	printPermissionsHeader(stdout, s, in)
	if cfg.Policy.Required {
		fmt.Fprintf(stdout, "Safety policy: on, and its denies stay on top of everything below (aggy policy --help).\n")
	} else {
		fmt.Fprintf(stdout, "Safety policy: on where a CLI can take it; [policy] required = false lets a CLI start without it.\n")
	}
	printRules(stdout, s)

	fmt.Fprintln(stdout, "\nWhat each CLI gets, at its next launch or switch")
	claude := s.Render(adapter.Claude)
	fmt.Fprintf(stdout, "  Claude Code: reads its own settings files, and takes %s from aggy\n", ruleCounts(claude.Allow, claude.Ask, claude.Deny))
	grok := s.Render(adapter.Grok)
	fmt.Fprintf(stdout, "  Grok Build: reads Claude's settings files itself, unfiltered, and takes %s from aggy\n", ruleCounts(grok.Allow, nil, grok.Deny))
	shared, openCodeSkipped := parity.OpenCodeShared(s)
	fmt.Fprintf(stdout, "  OpenCode: takes %s, exact commands only, besides its shipped list\n", ruleCounts(shared.Allow, shared.Ask, shared.Deny))
	fmt.Fprintln(stdout, "  Codex: takes none of these yet. It keeps asking in its own way, and its own")
	fmt.Fprintln(stdout, "    approvals still count; the hook that would answer for it is not built")

	printed := false
	skip := func(k permissions.Skip, on string) {
		if !printed {
			fmt.Fprintln(stdout, "\nSkipped")
			printed = true
		}
		fmt.Fprintf(stdout, "  %s %s  [%s]%s\n    %s\n", k.Kind, termsafe.Field(k.Text), originName(k.Origin), on, termsafe.Line(k.Why))
		if self := appliedBy(k.Origin); self != "" && on == "" {
			fmt.Fprintf(stdout, "    aggy does not pass this on, but%s, from its own file\n", self)
		}
	}
	for _, k := range s.Skipped {
		skip(k, "")
	}
	for _, k := range grok.Skipped {
		skip(k, " on Grok")
	}
	for _, k := range openCodeSkipped {
		skip(k, " on OpenCode")
	}
	printPermissionsTroubles(stdout, s)
	fmt.Fprintln(stdout, "\nNot shown: OpenCode's Always allow, which it keeps in opencode.db beside its sign-ins, and aggy never opens that.")
	return 0
}

// ruleCounts says how many rules of each kind.
func ruleCounts(allow, ask, deny []string) string {
	return fmt.Sprintf("%d allow, %d ask, and %d deny", len(allow), len(ask), len(deny))
}
