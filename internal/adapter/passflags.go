package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// PassFlags says what `aggy <vendor>` does with the CLI's own flags. Any
// flag it does not name goes to the CLI unchanged (InteractiveRequest.Extra);
// aggy does not list the flags a CLI has, so a new one needs no change here.
// Each adapter's table cites where its names come from.
type PassFlags struct {
	// Model, Continue, and Resume are the spellings aggy takes over and
	// does itself, so the index stays true: the model goes on the child's
	// row, and continue and resume pick from aggy's own sessions.
	Model, Continue, Resume []string
	// Refused are the flags that clash with what aggy passes at launch or
	// keep the session from being the one aggy recorded.
	Refused []RefusedFlag
	// Checked are flags with a value, which Check refuses or allows.
	Checked []CheckedFlag
	// Values are the other flags known to take a value as the next word.
	// A bare word after any other flag is the prompt, unless the CLI's own
	// help says the flag takes a value (ParseOpts.Values). --flag=value needs
	// no entry.
	Values []string
	// Variadic are flags that take every following word up to the next flag
	// as their values, as commander's <values...> does.
	Variadic []string
	// Print are the flags that make the CLI print one reply and exit (Claude
	// and Grok -p). Such a run is not a session aggy supervises or records:
	// aggy runs the CLI directly, with the flags and safety policy it adds on
	// screen (PrintRequested, ParseOpts.Print).
	Print []string
	// PrintCommands are subcommands that do the same, as the first word
	// (codex exec).
	PrintCommands []string
	// Bare are the flags that start no local session at all (claude
	// --desktop, --cloud, --version): aggy runs the CLI with the words as
	// given and adds nothing, since its launch flags could only break them.
	Bare []string
	// Commands are the CLI's own subcommand names. On screen a first prompt
	// word that names one is refused, since the CLI could run it instead of
	// the session aggy records.
	Commands []string
	// Once are flags that go to the first launch of a session only, not to a
	// later launch of it (ParsedFlags.Safe leaves them out).
	Once []string
	// Noted are flags that pass, with a line for the person at launch.
	Noted []NotedFlag
	// LoosensIf gates any flag whose name holds one of these words, for a CLI
	// with no list of its flags to compare a new one with.
	LoosensIf []string
	// DirFlags name a folder the session gets. They are also in Checked, so
	// their value is the next word. An adapter touches no disk, so Parse only
	// lists them (ParsedFlags.Dirs), for the launch to resolve against the
	// directory the CLI starts in.
	DirFlags []string
	// Owned are the flags aggy merges with its own or takes over (claude
	// --settings, grok --rules, codex -c). After "--" each is refused, in any
	// form (checkAfterDashes): there it would reach the CLI unmerged and
	// unchecked if the CLI did not take "--" as the end of its options.
	Owned []string
	// Effect says in plain words what a loosening flag, as LoosenedFlag
	// names it, does to aggy's safety policy on this CLI, for the question
	// aggy asks before letting it through (T6979). nil, or "", is the
	// generic line: aggy has not checked.
	Effect func(flag string) string
	// Only, when set, are the exact spellings of the CLI's own flags that may
	// pass on screen, besides Model, Continue, Resume, and Refused (which keep
	// their own handling): any other dash-led word is refused (ErrNotListed).
	// For a CLI whose parser expands abbreviations (Python's argparse, with
	// allow_abbrev), where --yol is --yolo and --api-ke=x is --api-key=x, so no
	// list of refused names can be complete (#134 review).
	Only []string
}

// ErrNotListed matches a flag that is not one of PassFlags.Only.
var ErrNotListed = errors.New("aggy passes only the flags it lists for this CLI, written in full: the CLI expands abbreviations, so an unlisted spelling could be a flag aggy refuses")

// LoosenEffect is p's line for a loosening flag: its Effect, or the generic
// one.
func (p PassFlags) LoosenEffect(flag string) string {
	if p.Effect != nil {
		if e := p.Effect(flag); e != "" {
			return e
		}
	}
	return "aggy has not checked what it does to its safety policy on this CLI, so treat the policy as off for what it allows"
}

// LoosenedFlag is the flag err refuses for ErrLoosens, as it names it (with
// the value, where the value is what loosens: --permission-mode
// bypassPermissions), or false for any other error.
func LoosenedFlag(err error) (string, bool) {
	if !errors.Is(err, ErrLoosens) {
		return "", false
	}
	f, ok := strings.CutSuffix(err.Error(), ": "+ErrLoosens.Error())
	return f, ok && f != ""
}

// DirFlag is one folder a flag on the command line names.
type DirFlag struct{ Name, Value string }

// NotedFlag is a flag that passes and is worth a line at launch: it changes
// something aggy relies on, but gating it would cost the whole safety policy
// for less than that policy gives.
type NotedFlag struct {
	Names []string
	Note  string
}

// RefusedFlag is a group of spellings refused for one reason.
type RefusedFlag struct {
	Names []string
	Why   string
	// UnlessNoPolicy allows the flag with --no-policy: it loosens
	// approvals, the sandbox, or hook trust (ErrLoosens).
	UnlessNoPolicy bool
	// ScreenOnly passes the flag in print mode (ParseOpts.Print): it clashes
	// only with the session aggy records on screen.
	ScreenOnly bool
}

// CheckedFlag is a flag whose value Check refuses or allows. Check wraps
// ErrLoosens for a value that --no-policy lets through.
type CheckedFlag struct {
	Names []string
	Check func(value string) error
}

// ParsedFlags is an `aggy <vendor>` command line after PassFlags.Parse.
type ParsedFlags struct {
	Model    string
	Continue bool
	Resume   bool
	ResumeID string // empty for a --resume with no id
	// Extra are the words for the CLI, in the order given, the prompt
	// included.
	Extra []string
	// Safe is Extra without the flags in Loosens and Once and without the
	// prompt, for a later launch of the session that does not have
	// --no-policy and must not send the prompt again.
	Safe []string
	// Prompt are the words of Extra that are the prompt.
	Prompt []string
	// Print says a Print flag was given.
	Print bool
	// Once names the flags of PassFlags.Once given.
	Once []string
	// Loosens names the flags --no-policy let through.
	Loosens []string
	// Notes are the lines the flags in Noted ask for, each once.
	Notes []string
	// Dirs are the values of DirFlags, as typed.
	Dirs []DirFlag
}

// ErrLoosens matches a flag that loosens approvals, the sandbox, or hook
// trust, which aggy passes only with --no-policy.
var ErrLoosens = errors.New("it loosens approvals, the sandbox, or hook trust, or aggy cannot tell that it does not; add --no-policy to start the session that way")

// ErrNotFlag matches a word on the command line that aggy will not pass as a
// prompt: one that names a subcommand of the CLI, which the CLI could run
// instead of a session.
var ErrNotFlag = errors.New("it names a subcommand of the CLI, and aggy starts a session there, not the subcommand: run the CLI itself for that subcommand, or, for a prompt that starts with this word, put the prompt after -- to send it as text")

// ErrCommandAfterDashes matches a first word after "--" that names a
// subcommand of the CLI.
var ErrCommandAfterDashes = errors.New("it names a subcommand of the CLI, and aggy has not checked that the CLI sends it as text after --; start the prompt with another word")

// ErrDashesValue matches a "--" the CLI may read as a flag's value rather
// than the end of its options (PR #124 re-review): a commander option that
// takes a value takes the next word even when it is "--", and then reads
// every word after it as options.
var ErrDashesValue = errors.New("the CLI may read this -- as that flag's value and every word after it as its own options; give the flag its value, use the --flag=value form, or drop the --")

// ErrOwnedAfterDashes matches a flag after "--" that aggy merges or takes
// over: if the CLI read it, aggy's own copy would not be the one that holds.
var ErrOwnedAfterDashes = errors.New("aggy merges or sets this flag itself, so it may not come after --; put it before the --")

// ParseOpts are the options of ParseWith.
type ParseOpts struct {
	// NoPolicy lets through the flags that loosen approvals, the sandbox, or
	// hook trust (ErrLoosens), and lists them in Loosens.
	NoPolicy bool
	// Allowed are loosening flags, as LoosenedFlag names them, the person
	// let through one by one at a terminal (T6979): each passes, listed in
	// Loosens, and the rest of the policy stays.
	Allowed []string
	// Print parses for a run aggy neither supervises nor records (print mode,
	// a stdin that is no terminal): aggy takes over no flag, so the model,
	// continue, and resume flags go to the CLI as typed, and a RefusedFlag
	// with ScreenOnly passes.
	Print bool
	// Values are more flags known to take the next word as their value: the
	// probe's reading of the CLI's own help (adapter.Probe.ValueFlags). A
	// name ending in "..." takes every word up to the next flag.
	Values []string
	// Switches are the flags the same help shows with no value
	// (adapter.Probe.Switches). Of the flags with no value attached, only
	// these, in their exact spelling, may stand before a "--" (ParseWith).
	Switches []string
}

// Parse is ParseWith for an on-screen session.
func (p PassFlags) Parse(args []string, noPolicy bool) (ParsedFlags, error) {
	return p.ParseWith(args, ParseOpts{NoPolicy: noPolicy})
}

// RunMode is how aggy runs a CLI's words (PassFlags.Mode).
type RunMode int

// Run modes.
const (
	// RunScreen is an on-screen session under the supervisor, recorded.
	RunScreen RunMode = iota
	// RunPrint runs the CLI directly with aggy's launch flags and policy, and
	// records nothing: print mode, and the flags whose session aggy cannot
	// supervise (Print, PrintCommands).
	RunPrint
	// RunBare runs the CLI directly with the words as given and nothing
	// added: a flag that starts no local session (Bare).
	RunBare
)

// Mode says how aggy runs args: RunBare only when args are one Bare flag and
// nothing else (claude --version), RunPrint when they hold a Print flag or
// start with one of the PrintCommands, else RunScreen.
//
// aggy never guesses how the CLI parses its words in a way that would drop its
// policy (PR #124 review): a short-flag cluster (-nv) is not split to find a
// mode, since the CLI may read it as a flag with a value; a Bare flag with any
// other word is no bare run, whatever the CLI makes of it. Both other modes
// add aggy's policy, so a wrong guess between them (a Print flag's spelling
// read as some flag's value) costs only whether the run is recorded. Nothing
// after "--" counts.
func (p PassFlags) Mode(args []string) RunMode {
	if len(args) == 1 && slices.Contains(p.Bare, args[0]) {
		return RunBare
	}
	mode := RunScreen
	if len(args) > 0 && slices.Contains(p.PrintCommands, args[0]) {
		mode = RunPrint
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		if name, _, _ := strings.Cut(a, "="); strings.HasPrefix(a, "-") && slices.Contains(p.Print, name) {
			mode = RunPrint
		}
	}
	return mode
}

// shortsOf is name, or each of the short flags run together in it (-dp).
func shortsOf(name string) []string {
	if strings.HasPrefix(name, "--") || len(name) <= 2 {
		return []string{name}
	}
	var out []string
	for _, r := range name[1:] {
		out = append(out, "-"+string(r))
	}
	return out
}

// valueKind says how many of the next words a flag takes as its value.
type valueKind int

const (
	valueNone valueKind = iota
	valueOne
	valueMany // every word up to the next flag, as commander's <values...>
)

// takesValue says whether name takes the next word as its value, by the table
// (Values, Variadic, Checked) or by the CLI's help (values).
func (p PassFlags) takesValue(name string, values []string) valueKind {
	switch {
	case slices.Contains(p.Variadic, name) || slices.Contains(values, name+"..."):
		return valueMany
	case slices.Contains(p.Values, name) || slices.Contains(values, name):
		return valueOne
	}
	if _, ok := p.checked(name); ok {
		return valueOne
	}
	return valueNone
}

// ParseWith sorts args, the CLI's flags and prompt without aggy's own.
//
// A bare word directly after a flag that takes a value (the table's Values,
// Variadic, and Checked, and o.Values from the CLI's help) is that value.
// Any other bare word is the prompt, as the CLI itself reads it: it goes to
// the CLI in place, and Prompt lists it. "--" and every word after it go to
// the CLI as typed and are prompt too, once checkAfterDashes passes them. On screen, a first prompt word that
// names one of the CLI's Commands is refused (ErrNotFlag): the CLI could run
// that subcommand instead of the session aggy records.
//
// A flag naming "dangerously" is taken to loosen something on every vendor, so
// a new one needs no table entry. A value that looks like a refused flag is
// refused with it: the safe side, when aggy does not know every flag.
func (p PassFlags) ParseWith(args []string, o ParseOpts) (ParsedFlags, error) {
	var out ParsedFlags
	keep := func(words ...string) {
		out.Extra, out.Safe = append(out.Extra, words...), append(out.Safe, words...)
	}
	after := valueNone // what the flag before takes
	valueSafe := true  // whether that flag, so its value, is in Safe
	printCmd := false  // the first word is one of PrintCommands
	value := func(a string) {
		out.Extra = append(out.Extra, a)
		if valueSafe {
			out.Safe = append(out.Safe, a)
		}
	}
	// Rule A of the PR #124 re-review: a "--" ends the options only where the
	// CLI cannot read it as a value. unsure is the first word before it that
	// leaves that open (a flag that takes or may take a value with none
	// attached, one the CLI's help does not show as a switch, a prompt word),
	// and pending the flag still waiting for its value.
	unsure, pending := "", ""
	doubt := func(w string) {
		if unsure == "" {
			unsure = w
		}
	}
	prompted := false // a first prompt word that is not blank was seen
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			switch {
			case after != valueNone:
				return out, fmt.Errorf("-- after %s: %w", pending, ErrDashesValue)
			case unsure != "":
				return out, fmt.Errorf("-- after %q: %w", unsure, ErrDashesValue)
			}
			loosens, err := p.checkAfterDashes(args[i+1:], o)
			if err != nil {
				return out, err
			}
			out.Loosens = append(out.Loosens, loosens...)
			out.Extra = append(out.Extra, args[i:]...)
			out.Prompt = append(out.Prompt, args[i+1:]...)
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			switch {
			case after == valueOne:
				value(a)
				after = valueNone
			case after == valueMany:
				value(a)
			case i == 0 && slices.Contains(p.PrintCommands, a):
				// codex exec: the print subcommand itself, whose own
				// arguments the CLI's top-level Commands do not name.
				keep(a)
				printCmd = true
			case !printCmd && !prompted && p.isCommand(a):
				// In print mode too (PR #124 review): claude -p ultrareview
				// would run the subcommand, not a reply. Any case, and past
				// blank words (PR #124 re-review).
				return out, fmt.Errorf("%q: %w", a, ErrNotFlag)
			default:
				out.Extra = append(out.Extra, a)
				out.Prompt = append(out.Prompt, a)
				prompted = prompted || strings.TrimSpace(a) != ""
				doubt(a)
			}
			continue
		}
		// A dash-led word is always checked as a flag, even where the flag
		// before it waits for a value: the CLI may take it as that value,
		// which drops it, but aggy never lets it through unchecked.
		after = valueNone
		name, val, hasVal := p.split(a)
		switch {
		case slices.Contains(p.Model, name):
			words := []string{a}
			if !hasVal {
				if i+1 == len(args) {
					return out, fmt.Errorf("%s needs a model name", name)
				}
				i++
				val = args[i]
				words = append(words, val)
			}
			if err := ValidModel(val); err != nil || val == "" {
				return out, fmt.Errorf("%q is not a model name", val)
			}
			out.Model = val
			if o.Print {
				keep(words...)
			}
		case slices.Contains(p.Continue, name):
			out.Continue = true
			if !slices.Contains(o.Switches, a) {
				doubt(a)
			}
			if o.Print {
				keep(a)
			}
		case slices.Contains(p.Resume, name):
			out.Resume = true
			words := []string{a}
			if !hasVal && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				val = args[i]
				words = append(words, val)
			}
			if !hasVal && len(words) == 1 {
				doubt(a) // an optional value, which the CLI may give "--"
			}
			out.ResumeID = val
			if o.Print {
				keep(words...)
			}
		default:
			before := i
			loosens, err := p.checkPassed(args, &i, name, val, hasVal, o)
			if err != nil {
				return out, err
			}
			if slices.Contains(p.Print, name) || slices.Contains(p.Bare, name) {
				out.Print = true
			}
			for _, n := range p.Noted {
				if slices.Contains(n.Names, name) && !slices.Contains(out.Notes, n.Note) {
					out.Notes = append(out.Notes, n.Note)
				}
			}
			words := args[before : i+1]
			if slices.Contains(p.DirFlags, name) {
				dir := val
				if !hasVal {
					dir = args[i]
				}
				out.Dirs = append(out.Dirs, DirFlag{Name: name, Value: dir})
			}
			out.Extra = append(out.Extra, words...)
			valueSafe = false
			switch {
			case loosens:
				out.Loosens = append(out.Loosens, name)
			case slices.Contains(p.Once, name):
				out.Once = append(out.Once, name)
			default:
				out.Safe = append(out.Safe, words...)
				valueSafe = true
			}
			if !hasVal && i == before {
				after = p.takesValue(name, o.Values)
				switch {
				case after != valueNone:
					pending = name
					if after == valueMany {
						doubt(a) // its list may run on past a "--" on some parsers
					}
				case !slices.Contains(o.Switches, a):
					// Not shown as a switch in exactly this spelling: it may
					// take a value (an unknown flag, a cluster, an optional
					// value), so a "--" after it may be that value.
					doubt(a)
				}
			}
		}
	}
	return out, nil
}

// owned reports whether name, or any short flag run together in it, is one
// aggy merges or takes over (Owned, Model, Continue, Resume).
func (p PassFlags) owned(name string) bool {
	for _, n := range append([]string{name}, shortsOf(name)...) {
		if slices.Contains(p.Owned, n) || slices.Contains(p.Model, n) || slices.Contains(p.Continue, n) || slices.Contains(p.Resume, n) {
			return true
		}
	}
	return false
}

// isCommand reports whether w names one of Commands, in any case.
func (p PassFlags) isCommand(w string) bool {
	return slices.ContainsFunc(p.Commands, func(c string) bool { return strings.EqualFold(w, c) })
}

// checkAfterDashes checks the words after "--" (PR #124 re-reviews), as a
// defence that holds even if ParseWith's rule A misjudged the "--" and the CLI
// reads these words as options. aggy does not rely on the CLI reading them as
// prompt: a CLI that still dispatched "-- agents" or parsed "--
// --dangerously-skip-permissions" would get past the policy. So:
//
//   - The first word that is neither blank nor dash-led may not name one of
//     Commands, in any case; nor may one that directly follows a dash-led word
//     before it (it could be that flag's value, and the next word the
//     subcommand), up to the first that does not.
//   - Every dash-led word gets the same checks as one before "--" (Refused,
//     Gated, Checked, LoosensIf, "=" forms included); it returns the ones
//     --no-policy lets through. A Checked flag with no word after it is left
//     to the CLI, which has no value to give it.
//   - A flag aggy merges or takes over (Owned, Model, Continue, Resume) is
//     refused there outright, in any form, as is a short-flag cluster holding
//     one.
//
// TODO(T6895): a live check that each CLI reads "-- doctor" as a prompt
// (claude -- doctor) would let the first-word rule be loosened. Commands is
// the reference's list: a subcommand a later CLI adds and the table does not
// list is not caught here.
func (p PassFlags) checkAfterDashes(words []string, o ParseOpts) ([]string, error) {
	afterFlag := false
	for _, w := range words {
		switch {
		case w != "-" && strings.HasPrefix(w, "-"):
			afterFlag = true
			continue
		case strings.TrimSpace(w) == "":
			continue
		case p.isCommand(w):
			return nil, fmt.Errorf("%q after --: %w", w, ErrCommandAfterDashes)
		}
		if !afterFlag {
			break
		}
		afterFlag = false
	}
	var loosens []string
	for j := 0; j < len(words); j++ {
		a := words[j]
		if a == "-" || a == "--" || !strings.HasPrefix(a, "-") {
			continue
		}
		name, val, hasVal := p.split(a)
		if p.owned(name) {
			return nil, fmt.Errorf("%s after --: %w", name, ErrOwnedAfterDashes)
		}
		if _, ok := p.checked(name); ok && !hasVal && j+1 == len(words) {
			continue
		}
		l, err := p.checkPassed(words, &j, name, val, hasVal, o)
		if err != nil {
			return nil, err
		}
		if l {
			loosens = append(loosens, name)
		}
	}
	return loosens, nil
}

// split cuts a flag from its value: --name=value, -n=value, or -nvalue for
// a one-letter flag aggy takes or checks. Another short flag stays whole,
// for checkPassed to look through.
func (p PassFlags) split(a string) (name, val string, hasVal bool) {
	if strings.HasPrefix(a, "--") {
		return strings.Cut(a, "=")
	}
	if len(a) > 2 {
		short := a[:2]
		_, checked := p.checked(short)
		if checked || slices.Contains(p.Model, short) || slices.Contains(p.Resume, short) {
			return short, strings.TrimPrefix(a[2:], "="), true
		}
	}
	return a, "", false
}

// listed refuses, where Only is set and the run is on screen, a flag that is
// neither in Only nor a Refused spelling (which keeps its own reason). The
// error names the flag without its value: --api-ke=sk-x is --api-ke, and a
// short word with letters run on, which may be a value (-psk-x), only its
// first letter.
func (p PassFlags) listed(name string, o ParseOpts) error {
	if len(p.Only) == 0 || o.Print || slices.Contains(p.Only, name) || p.refusedName(name) {
		return nil
	}
	shown := name
	if !strings.HasPrefix(name, "--") && len(name) > 2 {
		shown = name[:2] + "..."
	}
	if strings.HasPrefix(name, "--") {
		for _, r := range p.Refused {
			for _, n := range r.Names {
				if strings.HasPrefix(n, name) && n != name {
					return fmt.Errorf("%s (perhaps %s, abbreviated): %w", shown, n, ErrNotListed)
				}
			}
		}
	}
	return fmt.Errorf("%s: %w", shown, ErrNotListed)
}

// abbreviatesLoosening reports, for a CLI with Only set, a long flag that is a
// strict prefix of a refused flag that loosens (--yol for --yolo): its parser
// would expand it.
func (p PassFlags) abbreviatesLoosening(name string) bool {
	if len(p.Only) == 0 || !strings.HasPrefix(name, "--") || len(name) < 3 {
		return false
	}
	return slices.ContainsFunc(p.Refused, func(r RefusedFlag) bool {
		return r.UnlessNoPolicy && slices.ContainsFunc(r.Names, func(n string) bool { return n != name && strings.HasPrefix(n, name) })
	})
}

// refusedName reports a spelling one of Refused names exactly.
func (p PassFlags) refusedName(name string) bool {
	return slices.ContainsFunc(p.Refused, func(r RefusedFlag) bool { return slices.Contains(r.Names, name) })
}

// checked finds name's CheckedFlag.
func (p PassFlags) checked(name string) (CheckedFlag, bool) {
	for _, c := range p.Checked {
		if slices.Contains(c.Names, name) {
			return c, true
		}
	}
	return CheckedFlag{}, false
}

// checkPassed refuses a flag that may not go to the CLI, and reports one
// that --no-policy lets through. A CheckedFlag with its value in the next
// argument moves *i to that value.
func (p PassFlags) checkPassed(args []string, i *int, name, val string, hasVal bool, o ParseOpts) (bool, error) {
	gate := func(err error) (bool, error) {
		if errors.Is(err, ErrLoosens) && o.NoPolicy {
			return true, nil
		}
		if f, ok := LoosenedFlag(err); ok && slices.Contains(o.Allowed, f) {
			return true, nil
		}
		return false, err
	}
	if err := p.listed(name, o); err != nil {
		return false, err
	}
	if c, ok := p.checked(name); ok {
		if !hasVal {
			if *i+1 == len(args) {
				return false, fmt.Errorf("%s needs a value", name)
			}
			*i++
			val = args[*i]
			if val == "--" {
				// The CLI takes it as the value and reads the words after it
				// as options; aggy would have to as well (PR #124 re-review).
				return false, fmt.Errorf("-- after %s: %w", name, ErrDashesValue)
			}
		}
		return gate(c.Check(val))
	}
	if strings.Contains(name, "dangerously") {
		return gate(fmt.Errorf("%s: %w", name, ErrLoosens))
	}
	for _, w := range p.LoosensIf {
		if strings.Contains(name, w) {
			return gate(fmt.Errorf("%s: %w", name, ErrLoosens))
		}
	}
	// Short flags run together, as -cp: any of them may clash.
	names := shortsOf(name)
	loosens := false
	for _, n := range names {
		if len(names) > 1 && (slices.Contains(p.Model, n) || slices.Contains(p.Continue, n) || slices.Contains(p.Resume, n)) {
			return false, fmt.Errorf("write %s on its own, not as part of %s", n, name)
		}
		for _, r := range p.Refused {
			if !slices.Contains(r.Names, n) || o.Print && r.ScreenOnly {
				continue
			}
			if !r.UnlessNoPolicy {
				return false, fmt.Errorf("%s clashes with aggy: %s", n, r.Why)
			}
			if l, err := gate(fmt.Errorf("%s: %w", n, ErrLoosens)); err != nil {
				return false, err
			} else if l {
				loosens = true
			}
		}
	}
	return loosens, nil
}

// Gated refuses the words of a subcommand aggy runs as typed (T6963) when any
// of them is a flag the table gates: one that loosens approvals, the sandbox,
// or hook trust (a refused UnlessNoPolicy flag, "dangerously", LoosensIf), a
// Checked flag whose value its Check refuses, or one of extra (flags that carry
// a policy of their own, such as claude --settings). Such a run gets no policy
// from aggy, so --no-policy has nothing to let through and the flag is refused
// outright. A flag clashing only with aggy's session passes.
//
// The words after "--" are looked at too (PR #124 re-review, rule B): they are
// usually the subcommand's own command (claude mcp add name -- cmd args), but
// a "--" the CLI read as a flag's value would make them its options. So an MCP
// server command whose own arguments hold such a flag is refused; run the CLI
// itself for it.
func (p PassFlags) Gated(words []string, extra ...string) error {
	for i := 0; i < len(words); i++ {
		a := words[i]
		if a == "-" || a == "--" || !strings.HasPrefix(a, "-") {
			continue
		}
		name, val, hasVal := p.split(a)
		if slices.Contains(extra, name) || p.abbreviatesLoosening(name) {
			return fmt.Errorf("%s: %w", name, ErrSubcommandGated)
		}
		if _, err := p.checkPassed(words, &i, name, val, hasVal, ParseOpts{Print: true}); errors.Is(err, ErrLoosens) || err != nil && p.isChecked(name) {
			return fmt.Errorf("%s: %w", name, ErrSubcommandGated)
		}
	}
	return nil
}

// ErrSubcommandGated matches a flag Gated refuses.
var ErrSubcommandGated = errors.New("aggy adds no policy to a subcommand it runs as typed, so it passes no flag that loosens or sets one")

// isChecked reports whether name is a Checked flag.
func (p PassFlags) isChecked(name string) bool {
	_, ok := p.checked(name)
	return ok
}

// TakeFlag takes every one of names out of words, written as two words or
// with an =, and returns their values in order and the words left. A name
// with no word after it stays in rest.
//
// It looks past "--" too (PR #124 re-review, rule C): a "--" the CLI takes as
// a flag's value is no end of options, and a --settings after it would then
// replace aggy's own layer. ParseWith refuses these names after a "--" it
// takes as the end of options (PassFlags.Owned), so no prompt text is taken.
// It also takes one that follows a flag aggy cannot tell takes a value
// (--unknown --settings x): the CLI may read it either way, and aggy then
// merges and checks it rather than let it pass.
func TakeFlag(words []string, names ...string) (vals, rest []string) {
	for i := 0; i < len(words); i++ {
		w := words[i]
		name, val, hasVal := strings.Cut(w, "=")
		switch {
		case !slices.Contains(names, name):
			rest = append(rest, w)
		case hasVal:
			vals = append(vals, val)
		case i+1 < len(words):
			i++
			vals = append(vals, words[i])
		default:
			rest = append(rest, w)
		}
	}
	return vals, rest
}

// AnyValue is the Check of a flag that takes any value but another flag: listed
// in Checked, its next word is its value, so --enable --yolo must not hide a flag
// from the checks. A real value that starts with - is refused with it, and the
// message says so.
func AnyValue(val string) error {
	if strings.HasPrefix(val, "-") {
		return fmt.Errorf("%q looks like a flag where aggy wants this flag's value", val)
	}
	return nil
}

// CheckAgents reads an --agents value, which defines subagents inline, through
// the JSON decoder, so an escape cannot hide a word. The value of every key that
// normalizes to permissionMode, at any depth, goes through checkMode, and
// bypassPermissions as any string counts too. All of a repeated key is seen, since
// CLIs differ on which wins. A value that is not a JSON object, such as a file's
// path, cannot be inspected, so it loosens as far as aggy can tell.
func CheckAgents(val string, checkMode func(string) error) error {
	gated := fmt.Errorf("--agents: %w", ErrLoosens)
	// The decoder's Token takes text cut short for a clean end.
	if !json.Valid([]byte(val)) {
		return gated
	}
	dec := json.NewDecoder(strings.NewReader(val))
	first, err := dec.Token()
	if d, ok := first.(json.Delim); err != nil || !ok || d != '{' {
		return gated
	}
	// One frame per open container, so a word is a key only where a key goes:
	// a value that says permissionMode is a value.
	type frame struct{ object, wantKey bool }
	stack := []frame{{object: true, wantKey: true}}
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
	afterMode := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return gated
		}
		d, isDelim := tok.(json.Delim)
		if isDelim && (d == '}' || d == ']') {
			stack = stack[:len(stack)-1]
			valueDone()
			continue
		}
		s, isString := tok.(string)
		if isString && strings.EqualFold(s, "bypassPermissions") {
			return gated
		}
		if top := &stack[len(stack)-1]; top.object && top.wantKey {
			top.wantKey = false
			afterMode = strings.EqualFold(strings.NewReplacer("-", "", "_", "").Replace(s), "permissionMode")
			continue
		}
		if afterMode && (!isString || checkMode(s) != nil) {
			return gated
		}
		afterMode = false
		if isDelim {
			stack = append(stack, frame{object: d == '{', wantKey: d == '{'})
			continue
		}
		valueDone()
	}
}
