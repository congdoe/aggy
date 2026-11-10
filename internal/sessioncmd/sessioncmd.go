// Package sessioncmd parses the commands a person types inside a session aggy
// supervises (docs/design.md, section 5, "Commands inside a session"): /switch,
// /borrow, /inbox, and /aggy ..., and on Codex their $ forms too.
//
// It only parses. The supervisor catches a line from the keystrokes it passes
// through, and aggy hook catches one from a CLI's UserPromptSubmit hook; both
// parse it here, and the supervisor parses it again, never trusting a hook's
// word. A line this package does not take is no command: it goes to the CLI as
// typed, so a miss is harmless. Only a command that starts no model turn is
// taken: /borrow with a task, and /aggy with a question, go to the model.
package sessioncmd

import (
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/tooldef"
)

// MaxLine bounds a command line, in bytes. A longer line is no command.
const MaxLine = 256

// maxToken bounds one word of a command.
const maxToken = 128

// Kind is what a command does.
type Kind int

// The kinds.
const (
	// Picker opens the switch picker (/switch).
	Picker Kind = iota + 1
	// Switch queues a switch to Vendor (/switch <vendor> [--brief] [--new]
	// [--model <id>]).
	Switch
	// BorrowList lists the borrow kinds (/borrow): picking one fills in the
	// rest of a /borrow line, which goes to the model.
	BorrowList
	// AggyList lists every command (/aggy).
	AggyList
	// ToolList lists the managed tools to turn on or off (/aggy tool).
	ToolList
	// Run runs one of aggy's own commands, Args, and shows what it printed.
	Run
	// TrustList opens this folder's trust list (/aggy trust, T6977): the
	// person ticks rows on aggy's own screen, and confirming records them.
	TrustList
)

// Command is one parsed command.
type Command struct {
	Kind Kind
	// Name is a fixed name for records and screens, such as "switch" or
	// "aggy usage": never the person's arguments.
	Name string
	// Switch: the vendor and the options for that one launch.
	Vendor adapter.Vendor
	Brief  bool
	New    bool
	Model  string
	// Run: aggy's arguments. Sub names the subcommand; "brief" and "log"
	// take the session's own ids, which the supervisor fills in. Text is
	// shown instead of running anything, when set.
	Sub  string
	Args []string
	Text string
	// Confirm, when set, is what runs once the person presses y on the
	// result of Args, which showed what it would do; ConfirmLabel names it.
	// Answer: that run answers the command's own question with y.
	Confirm      []string
	ConfirmLabel string
	Answer       bool
	// ConfirmIf, when set, keeps the y step only on a page whose first run
	// printed it: tool on asks a question only for a first index (T6975).
	ConfirmIf string
	// Also, when set, is a step the person may take first with c, before y,
	// on that same page; AlsoLabel names it. accept offers the repo's checks
	// so, as accept at a terminal does (R121).
	Also      []string
	AlsoLabel string
	// KeyboardOnly: it changes something, so it is taken only from the
	// keyboard, never from a hook, which any process in the CLI's tree can
	// send. No skill the model can call runs it.
	KeyboardOnly bool
}

// Prefixes returns the characters a command may start with on v: "/" on every
// CLI, and also "$" on Codex, whose menus list skills as $name and which has
// no custom / commands at rust-v0.164.0 (docs/design.md, section 5).
func Prefixes(v adapter.Vendor) string {
	if v == adapter.Codex {
		return "/$"
	}
	return "/"
}

// Prefix is the form v's menu shows a command in: $ on Codex, else /.
func Prefix(v adapter.Vendor) string {
	if v == adapter.Codex {
		return "$"
	}
	return "/"
}

// Parse reads line, as typed or submitted on v, and returns the command it
// is. ok is false for anything else, a command with a model turn included.
func Parse(v adapter.Vendor, line string) (Command, bool) {
	line = strings.TrimRight(line, " ")
	if len(line) < 2 || len(line) > MaxLine || !strings.ContainsRune(Prefixes(v), rune(line[0])) {
		return Command{}, false
	}
	for i := 0; i < len(line); i++ {
		if line[i] < 0x20 || line[i] > 0x7e {
			return Command{}, false
		}
	}
	words := strings.Fields(line[1:])
	if len(words) == 0 || line[1] == ' ' {
		return Command{}, false
	}
	name, args := words[0], words[1:]
	for _, w := range args {
		if !plainWord(w) {
			return Command{}, false
		}
	}
	switch name {
	case "switch":
		return parseSwitch(args)
	case "borrow":
		if len(args) == 0 {
			return Command{Kind: BorrowList, Name: "borrow"}, true
		}
		return Command{}, false // a borrow is a model turn: its report lands in the conversation
	case "inbox":
		c, ok := inbox(args)
		c.Name = "inbox"
		return c, ok
	case "aggy":
		if len(args) == 0 {
			return Command{Kind: AggyList, Name: "aggy"}, true
		}
		return parseAggy(args[0], args[1:])
	}
	return Command{}, false
}

// plainWord reports whether w holds only characters that need no quoting and
// open no CLI popup: letters, digits, and . _ : / + = -.
func plainWord(w string) bool {
	if len(w) > maxToken {
		return false
	}
	for _, r := range w {
		if !wordRune(r, "._:/+=-") {
			return false
		}
	}
	return true
}

// wordRune reports whether r is an ASCII letter or digit, or one of extra.
func wordRune(r rune, extra string) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(extra, r)
}

// idWord reports whether w can be an id or an id's prefix.
func idWord(w string) bool {
	if w == "" || len(w) > 64 || w[0] == '-' {
		return false
	}
	for _, r := range w {
		if !wordRune(r, "-_") {
			return false
		}
	}
	return true
}

func parseSwitch(args []string) (Command, bool) {
	c := Command{Kind: Picker, Name: "switch"}
	if len(args) == 0 {
		return c, true
	}
	c.Kind = Switch
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--brief" && !c.Brief:
			c.Brief = true
		case a == "--new" && !c.New:
			c.New = true
		case a == "--model" && c.Model == "" && i+1 < len(args):
			i++
			if adapter.ValidModel(args[i]) != nil {
				return Command{}, false
			}
			c.Model = args[i]
		case strings.HasPrefix(a, "--model=") && c.Model == "":
			m := strings.TrimPrefix(a, "--model=")
			if adapter.ValidModel(m) != nil {
				return Command{}, false
			}
			c.Model = m
		case !strings.HasPrefix(a, "-") && c.Vendor == "":
			v, err := adapter.ParseVendor(a)
			if err != nil {
				return Command{}, false
			}
			c.Vendor = v
		default:
			return Command{}, false
		}
	}
	if c.Vendor == "" {
		return Command{}, false
	}
	return c, true
}

func inbox(args []string) (Command, bool) {
	c := Command{Kind: Run, Sub: "inbox", Args: []string{"inbox"}}
	for _, a := range args {
		switch {
		case a == "--all" && !slices.Contains(c.Args, "--all"):
			c.Args = append(c.Args, a)
		case idWord(a) && len(c.Args) == 1:
			c.Args = append(c.Args, a)
		default:
			return Command{}, false
		}
	}
	return c, true
}

// Asked is how aggy's own commands end a yes-or-no question they print.
const Asked = "[y/N]"

// Picked reports whether line, a command a CLI submitted and its prompt hook
// reported, can be the one the person's keys picked from the CLI's own menu
// (docs/design.md section 5, T6975). typed is what they typed at the prompt
// before any menu key (Up, Down, Tab), as printable keys only; tabbed, that a
// Tab came after it; after, what they typed after that Tab.
//
// typed must be v's prefix and at least one character of a command word: a
// bare / is a prefix of every command, the person's own included, so it
// matches none. While no space ends the word, the word must start the line's
// command word (/sw for /switch); once a space ends it, it must be that word
// whole, as no menu completes it then. The arguments typed (after that space,
// or after the Tab) must be the line's own, word for word: a Tab completes the
// command's name only, so a line with other arguments is not the person's.
func Picked(v adapter.Vendor, typed string, tabbed bool, after, line string) bool {
	if len(typed) < 2 || len(line) < 2 || typed[0] != line[0] || !strings.ContainsRune(Prefixes(v), rune(typed[0])) || line[1] == ' ' {
		return false
	}
	word, rest, spaced := strings.Cut(typed[1:], " ")
	words := strings.Fields(line[1:])
	switch {
	case word == "" || len(words) == 0:
		return false
	case spaced && (tabbed || word != words[0]):
		return false
	case !strings.HasPrefix(words[0], word):
		return false
	}
	return slices.Equal(strings.Fields(rest+" "+after), words[1:])
}

// Subcommands are the /aggy commands that start no turn, in the order the
// list shows them, with what each does.
var Subcommands = []struct{ Name, What string }{
	{"sessions", "every session, its directory and last vendor"},
	{"session", "the children of this session"},
	{"usage", "what each vendor has left"},
	{"stats", "finished borrows per kind and vendor"},
	{"log", "what happened in this session"},
	{"inbox", "borrow reports you have not read"},
	{"brief", "the handoff brief the next CLI would get"},
	{"doctor", "check the CLIs, keys, policy, and tools"},
	{"diag", "the diagnostic log; diag <id> for one run"},
	{"check", "run the repo's checks on an edit borrow: check <id>"},
	{"accept", "apply an edit borrow: accept <id>"},
	{"reject", "throw away an edit borrow: reject <id>"},
	{"tool", "turn a managed tool on or off"},
	{"permissions", "the permission rules aggy keeps in step"},
	{"trust", "this folder's trust list: what may run here, where code goes"},
}

// parseAggy reads /aggy <sub> args. A first word that is no subcommand, or a
// subcommand with other arguments, is a question for the model.
func parseAggy(sub string, args []string) (Command, bool) {
	c := Command{Kind: Run, Name: "aggy " + sub, Sub: sub}
	switch sub {
	case "sessions", "session", "usage", "stats", "doctor", "brief", "log":
		if len(args) > 0 {
			return Command{}, false
		}
		c.Args = []string{sub}
	case "inbox":
		r, ok := inbox(args)
		r.Name = c.Name
		return r, ok
	case "diag":
		switch {
		case len(args) == 0:
			c.Args = []string{"diag", "-n", "50"}
		case len(args) == 1 && idWord(args[0]):
			c.Args = []string{"diag", "show", args[0]}
		default:
			return Command{}, false
		}
	case "check", "accept", "reject":
		switch {
		case len(args) == 0:
			c.Args = []string{sub, "--help"} // what is left: the id
		case len(args) == 1 && idWord(args[0]):
			c.KeyboardOnly = true
			id := args[0]
			switch sub {
			case "check":
				c.Args, c.Confirm, c.ConfirmLabel = []string{"check", id}, []string{"check", id, "--yes"}, "y runs the repo's checks"
			case "accept":
				c.Args, c.Confirm = []string{"accept", id}, []string{"accept", id, "--yes"}
				c.ConfirmLabel = "y applies it, checks skipped"
				c.Also, c.AlsoLabel = []string{"check", id, "--yes"}, "c runs the checks first"
			case "reject":
				c.Text = "Reject edit borrow " + id + ": aggy removes its worktree and its branch."
				c.Confirm, c.ConfirmLabel = []string{"reject", id}, "y rejects it"
			}
		default:
			return Command{}, false
		}
	case "tool":
		switch {
		case len(args) == 0:
			return Command{Kind: ToolList, Name: c.Name, KeyboardOnly: true}, true
		case len(args) == 1 && args[0] == "status":
			c.Args = []string{"tool", "status"}
		case len(args) == 2 && (args[0] == "on" || args[0] == "off") && tooldef.ValidName(args[1]):
			c.Args, c.KeyboardOnly = []string{"tool", args[0], args[1]}, true
			if args[0] == "on" {
				// The first run answers a question it asks (a first index's
				// y/N) no, on the page; y runs it again answering yes, as at
				// a terminal (T6975). A run that asked nothing has no y step.
				c.Confirm, c.ConfirmLabel, c.Answer, c.ConfirmIf = c.Args, "y answers its question yes", true, Asked
			}
		default:
			return Command{}, false
		}
		c.KeyboardOnly = true
	case "permissions":
		switch {
		case len(args) == 0, len(args) == 1 && args[0] == "list":
			c.Args = []string{"permissions", "list"}
		case len(args) == 1 && args[0] == "show":
			c.Args = []string{"permissions", "show"}
		default:
			return Command{}, false
		}
		c.KeyboardOnly = true
	case "trust":
		switch {
		case len(args) == 0:
			// The folder's trust list, ticked on aggy's own screen (T6977).
			return Command{Kind: TrustList, Name: c.Name, KeyboardOnly: true}, true
		case len(args) == 1 && args[0] == "--opencode":
		default:
			return Command{}, false
		}
		// The shortcut that ticks OpenCode alone. The first run answers its
		// own question no, so it shows what trust does; y runs it again,
		// answering yes.
		c.Args = []string{"trust", "--opencode", "."}
		c.Confirm = c.Args
		c.ConfirmLabel, c.Answer, c.KeyboardOnly = "y trusts this folder", true, true
	default:
		return Command{}, false
	}
	return c, true
}
