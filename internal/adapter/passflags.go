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
var ErrNotFlag = errors.New("aggy passes a word there to the CLI as the session's first prompt, and this one names a subcommand of the CLI; put the prompt after -- to send it as text")

// ParseOpts are the options of ParseWith.
type ParseOpts struct {
	// NoPolicy lets through the flags that loosen approvals, the sandbox, or
	// hook trust (ErrLoosens), and lists them in Loosens.
	NoPolicy bool
	// Print parses for a run aggy neither supervises nor records (print mode,
	// a stdin that is no terminal): aggy takes over no flag, so the model,
	// continue, and resume flags go to the CLI as typed, and a RefusedFlag
	// with ScreenOnly passes.
	Print bool
	// Values are more flags known to take the next word as their value: the
	// probe's reading of the CLI's own help (adapter.Probe.ValueFlags). A
	// name ending in "..." takes every word up to the next flag.
	Values []string
}

// Parse is ParseWith for an on-screen session.
func (p PassFlags) Parse(args []string, noPolicy bool) (ParsedFlags, error) {
	return p.ParseWith(args, ParseOpts{NoPolicy: noPolicy})
}

// PrintRequested reports whether args hold one of the Print flags, or start
// with one of the PrintCommands: the run prints a reply and exits, so aggy
// runs it directly, with nothing to supervise or record. The value of a flag
// known to take one is skipped, and nothing after "--" counts.
func (p PassFlags) PrintRequested(args []string, values []string) bool {
	if len(args) > 0 && slices.Contains(p.PrintCommands, args[0]) {
		return true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return false
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, hasVal := p.split(a)
		for _, n := range shortsOf(name) {
			if slices.Contains(p.Print, n) {
				return true
			}
		}
		if !hasVal && (p.takesValue(name, values) == valueOne || slices.Contains(p.Model, name)) {
			i++
		}
	}
	return false
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
// the CLI as typed and are prompt too. On screen, a first prompt word that
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
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out.Extra = append(out.Extra, args[i:]...)
			out.Prompt = append(out.Prompt, args[i+1:]...)
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			switch {
			case after == valueOne:
				keep(a)
				after = valueNone
			case after == valueMany:
				keep(a)
			case !o.Print && len(out.Prompt) == 0 && slices.Contains(p.Commands, a):
				return out, fmt.Errorf("%q: %w", a, ErrNotFlag)
			default:
				out.Extra = append(out.Extra, a)
				out.Prompt = append(out.Prompt, a)
			}
			continue
		}
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
			for _, n := range shortsOf(name) {
				if slices.Contains(p.Print, n) {
					out.Print = true
				}
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
			switch {
			case loosens:
				out.Loosens = append(out.Loosens, name)
			case slices.Contains(p.Once, name):
				out.Once = append(out.Once, name)
			default:
				out.Safe = append(out.Safe, words...)
			}
			if !hasVal && i == before {
				after = p.takesValue(name, o.Values)
			}
		}
	}
	return out, nil
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
		return false, err
	}
	if c, ok := p.checked(name); ok {
		if !hasVal {
			if *i+1 == len(args) {
				return false, fmt.Errorf("%s needs a value", name)
			}
			*i++
			val = args[*i]
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
