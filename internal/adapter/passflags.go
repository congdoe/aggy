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
	// Checked are flags with a value, which Check refuses or allows.
	Checked []CheckedFlag
	// Values are the other flags known to take a value as the next word.
	// A bare word after any other flag is refused: it could be a prompt or
	// a subcommand. --flag=value needs no entry.
	Values []string
}

// RefusedFlag is a group of spellings refused for one reason.
type RefusedFlag struct {
	Names []string
	Why   string
	// UnlessNoPolicy allows the flag with --no-policy: it loosens
	// approvals, the sandbox, or hook trust (ErrLoosens).
	UnlessNoPolicy bool
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
	Extra    []string
	// Safe is Extra without the flags in Loosens, for a later launch of the
	// session that does not have --no-policy.
	Safe []string
	// Loosens names the flags --no-policy let through.
	Loosens []string
}

// ErrLoosens matches a flag that loosens approvals, the sandbox, or hook
// trust, which aggy passes only with --no-policy.
var ErrLoosens = errors.New("it loosens approvals, the sandbox, or hook trust, or aggy cannot tell that it does not; add --no-policy to start the session that way")

// ErrNotFlag matches a word on the command line that is not a flag or the
// value of one.
var ErrNotFlag = errors.New("aggy passes the CLI's flags, not a prompt or a subcommand; type the prompt once the session starts, or run the CLI itself for a subcommand")

// Parse sorts args, the CLI's flags without aggy's own. A bare word is
// passed only directly after a flag known to take a value (Values, Checked);
// anywhere else it could be a prompt or a subcommand. "--" is refused too,
// since all that follows it is prompt text. A flag naming "dangerously" is
// taken to loosen something on every vendor, so a new one needs no table
// entry. A value that looks like a refused flag is refused with it: the safe
// side, when aggy does not know every flag.
func (p PassFlags) Parse(args []string, noPolicy bool) (ParsedFlags, error) {
	var out ParsedFlags
	afterValueFlag := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return out, fmt.Errorf(`"--" starts a prompt: %w`, ErrNotFlag)
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			if !afterValueFlag {
				return out, fmt.Errorf("%q is not a flag or a value aggy knows the flag before it takes (for a value, write --flag=value): %w", a, ErrNotFlag)
			}
			out.Extra, out.Safe = append(out.Extra, a), append(out.Safe, a)
			afterValueFlag = false
			continue
		}
		afterValueFlag = false
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
			loosens, err := p.checkPassed(args, &i, name, val, hasVal, noPolicy)
			if err != nil {
				return out, err
			}
			words := args[before : i+1]
			out.Extra = append(out.Extra, words...)
			if loosens {
				out.Loosens = append(out.Loosens, name)
			} else {
				out.Safe = append(out.Safe, words...)
			}
			afterValueFlag = !hasVal && slices.Contains(p.Values, name)
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
func (p PassFlags) checkPassed(args []string, i *int, name, val string, hasVal, noPolicy bool) (bool, error) {
	gate := func(err error) (bool, error) {
		if errors.Is(err, ErrLoosens) && noPolicy {
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
	names := []string{name}
	if !strings.HasPrefix(name, "--") && len(name) > 2 {
		// Short flags run together, as -cp: any of them may clash.
		names = names[:0]
		for _, r := range name[1:] {
			names = append(names, "-"+string(r))
		}
	}
	loosens := false
	for _, n := range names {
		if slices.Contains(p.Model, n) || slices.Contains(p.Continue, n) || slices.Contains(p.Resume, n) {
			return false, fmt.Errorf("write %s on its own, not as part of %s", n, name)
		}
		for _, r := range p.Refused {
			if !slices.Contains(r.Names, n) {
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
