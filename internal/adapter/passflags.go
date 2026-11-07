package adapter

import (
	"errors"
	"fmt"
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
	// Checked are flags with a value that clashes only for some values.
	Checked []CheckedFlag
}

// RefusedFlag is a group of spellings refused for one reason.
type RefusedFlag struct {
	Names []string
	Why   string
	// UnlessNoPolicy allows the flag with --no-policy: it could take the
	// safety policy away, and that is what --no-policy asks for.
	UnlessNoPolicy bool
}

// CheckedFlag is a flag whose value Check refuses or allows.
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
	Extra    []string
}

// Parse sorts args, the CLI's flags without aggy's own. A bare word is
// passed only directly after a flag, as that flag's value: aggy cannot tell
// a value from a prompt or a subcommand, so anywhere else it is refused.
// "--" is refused too, since all that follows it is prompt text. A value
// that looks like a refused flag is refused with it: the safe side, when
// aggy does not know which flags take values.
func (p PassFlags) Parse(args []string, noPolicy bool) (ParsedFlags, error) {
	var out ParsedFlags
	afterFlag := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return out, errors.New(`"--" starts a prompt; type the prompt once the session starts`)
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			if !afterFlag {
				return out, fmt.Errorf("%q is not a flag: aggy passes the CLI's flags (one value after each), not a prompt or a subcommand; type the prompt once the session starts, or run the CLI itself for a subcommand", a)
			}
			out.Extra = append(out.Extra, a)
			afterFlag = false
			continue
		}
		afterFlag = false
		name, val, hasVal := p.split(a)
		switch {
		case slices.Contains(p.Model, name):
			if !hasVal {
				if i+1 == len(args) {
					return out, fmt.Errorf("%s needs a model name", name)
				}
				i++
				val = args[i]
			}
			if err := ValidModel(val); err != nil || val == "" {
				return out, fmt.Errorf("%q is not a model name", val)
			}
			out.Model = val
		case slices.Contains(p.Continue, name):
			out.Continue = true
		case slices.Contains(p.Resume, name):
			out.Resume = true
			if !hasVal && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				val = args[i]
			}
			out.ResumeID = val
		default:
			before := i
			if err := p.checkPassed(args, &i, name, val, hasVal, noPolicy); err != nil {
				return out, err
			}
			out.Extra = append(out.Extra, a)
			if i != before { // a checked flag's value, in the next argument
				out.Extra = append(out.Extra, args[i])
			} else if _, checked := p.checked(name); !checked {
				afterFlag = !hasVal
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

// checkPassed refuses a flag that may not go to the CLI. A CheckedFlag with
// its value in the next argument moves *i to that value.
func (p PassFlags) checkPassed(args []string, i *int, name, val string, hasVal, noPolicy bool) error {
	if c, ok := p.checked(name); ok {
		if !hasVal {
			if *i+1 == len(args) {
				return fmt.Errorf("%s needs a value", name)
			}
			*i++
			val = args[*i]
		}
		return c.Check(val)
	}
	names := []string{name}
	if !strings.HasPrefix(name, "--") && len(name) > 2 {
		// Short flags run together, as -cp: any of them may clash.
		names = names[:0]
		for _, r := range name[1:] {
			names = append(names, "-"+string(r))
		}
	}
	for _, n := range names {
		if slices.Contains(p.Model, n) || slices.Contains(p.Continue, n) || slices.Contains(p.Resume, n) {
			return fmt.Errorf("write %s on its own, not as part of %s", n, name)
		}
		for _, r := range p.Refused {
			if slices.Contains(r.Names, n) && (!r.UnlessNoPolicy || !noPolicy) {
				return fmt.Errorf("%s clashes with aggy: %s", n, r.Why)
			}
		}
	}
	return nil
}
