package policyhook

import (
	"slices"
	"strings"
)

// stripMessages returns cmd with the text of its message arguments blanked:
// the message of git commit or git tag (gitMessages) and the text of an echo
// or printf (echoText). They are text the command prints or stores, not a
// path it opens, so naming aggy's index there is no reason to ask (T6954).
// It is conservative: a command with a substitution ($(, `, <(, >(), a here
// document, or anything it cannot lex (an open quote) comes back whole, a
// redirection's target is never blanked, and anything it does not know
// leaves the words as they are.
func stripMessages(cmd string) string {
	for _, s := range []string{"$(", "`", "<(", ">(", "<<"} {
		if strings.Contains(cmd, s) {
			return cmd
		}
	}
	toks, ok := shellTokens(cmd)
	if !ok {
		return cmd
	}
	out := []byte(cmd)
	blank := func(t shellToken) {
		for i := t.start; i < t.end; i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	// Any pipe in the command (a subshell's included) may hand an echo's
	// text to something that runs it.
	piped := false
	for _, t := range toks {
		piped = piped || t.sep && (t.text == "|" || t.text == "|&")
	}
	for start := 0; start < len(toks); {
		end := start
		for end < len(toks) && !toks[end].sep {
			end++
		}
		stripSimple(toks[start:end], piped, blank)
		start = end + 1
	}
	return string(out)
}

// stripSimple blanks the message arguments of one simple command (words and
// redirections, no separators).
func stripSimple(toks []shellToken, piped bool, blank func(shellToken)) {
	// The words that are arguments: not a redirection, not its target, and
	// not a leading NAME=value. quiet says every redirection only goes to
	// /dev/null or another descriptor (>&2).
	var args []shellToken
	quiet := true
	var redir *shellToken
	for i, t := range toks {
		switch {
		case t.redir:
			redir = &toks[i]
		case redir != nil:
			dup := strings.HasSuffix(redir.text, "&") && (allDigits(t.value) || t.value == "-")
			quiet = quiet && (dup || t.value == "/dev/null" && !strings.HasPrefix(redir.text, "<"))
			redir = nil
		case len(args) == 0 && assignment(t.value):
		default:
			args = append(args, t)
		}
	}
	if len(args) == 0 || redir != nil {
		return
	}
	switch args[0].value {
	case "echo", "printf":
		if !piped && quiet {
			echoText(args[0].value, args[1:], blank)
		}
	case "git":
		gitMessages(args[1:], blank)
	}
}

// echoText blanks an echo's or printf's text: everything after echo's own
// options (-n, -e, -E, in any cluster), or all of printf's arguments. An
// option it does not know, and printf's -v (which sets a variable), leave
// the words as they are.
func echoText(cmd string, args []shellToken, blank func(shellToken)) {
	i := 0
	for ; i < len(args); i++ {
		v := args[i].value
		if len(v) < 2 || v[0] != '-' {
			break
		}
		if cmd == "printf" || strings.Trim(v[1:], "neE") != "" {
			return
		}
	}
	for _, a := range args[i:] {
		blank(a)
	}
}

// gitGlobals are git's options before its command (git(1)); value takes
// the next word too. Any other option leaves the command as it is.
var gitGlobals = map[string]bool{
	"-c": true, "-C": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--config-env": true, "--super-prefix": true,
	"-p": false, "--paginate": false, "-P": false, "--no-pager": false, "--no-replace-objects": false, "--no-lazy-fetch": false,
	"--no-optional-locks": false, "--no-advice": false, "--bare": false, "--literal-pathspecs": false, "--glob-pathspecs": false,
	"--noglob-pathspecs": false, "--icase-pathspecs": false,
}

// gitMsgOpts are the short options of git commit and git tag, by command
// (git-commit(1) and git-tag(1), SYNOPSIS and OPTIONS): flags take nothing,
// value takes the rest of the word or else the next word, and optional takes
// only the rest of the word. m is the message. long are the long options that
// take the next word as a value; a long option in neither list, unless given
// with =, leaves the rest of the command as it is.
var gitMsgOpts = map[string]struct {
	flags, value, optional string
	long, longFlags        []string
}{
	"commit": {flags: "apsnqveioz", value: "FCct", optional: "uS",
		long: []string{"--file", "--reuse-message", "--reedit-message", "--template", "--author", "--date", "--cleanup", "--fixup", "--squash", "--trailer", "--pathspec-from-file"},
		longFlags: []string{"--all", "--patch", "--signoff", "--no-signoff", "--no-verify", "--verify", "--quiet", "--verbose", "--edit", "--no-edit",
			"--include", "--only", "--amend", "--no-post-rewrite", "--reset-author", "--allow-empty", "--allow-empty-message", "--dry-run",
			"--short", "--branch", "--porcelain", "--long", "--null", "--status", "--no-status", "--interactive", "--no-gpg-sign", "--gpg-sign", "--pathspec-file-nul"}},
	"tag": {flags: "asfdvlie", value: "Fu", optional: "n",
		long: []string{"--file", "--local-user", "--cleanup", "--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--sort", "--format"},
		longFlags: []string{"--annotate", "--sign", "--no-sign", "--force", "--delete", "--verify", "--list", "--edit", "--no-edit", "--ignore-case",
			"--create-reflog", "--omit-empty"}},
}

// gitMessages blanks the message of git commit or git tag: -m <msg>, -m<msg>,
// --message <msg>, --message=<msg>, and a cluster of the command's flags
// ending in m (-am <msg>) or holding it (-am<msg>). The command is the first
// word after git's own options; a word it cannot place stops it.
func gitMessages(args []shellToken, blank func(shellToken)) {
	i := 0
	for ; i < len(args); i++ {
		v := args[i].value
		if !strings.HasPrefix(v, "-") {
			break
		}
		name, _, eq := strings.Cut(v, "=")
		value, ok := gitGlobals[name]
		switch {
		case !ok:
			return
		case value && !eq:
			i++
		}
	}
	if i >= len(args) {
		return
	}
	opts, ok := gitMsgOpts[args[i].value]
	if !ok {
		return
	}
	for i++; i < len(args); i++ {
		v := args[i].value
		switch {
		case v == "--":
			return
		case v == "-m" || v == "--message":
			if i+1 < len(args) {
				blank(args[i+1])
				i++
			}
		case strings.HasPrefix(v, "--message="):
			blank(args[i])
		case strings.HasPrefix(v, "--"):
			name, _, eq := strings.Cut(v, "=")
			switch {
			case eq:
			case slices.Contains(opts.long, name):
				i++
			case !slices.Contains(opts.longFlags, name):
				return
			}
		case strings.HasPrefix(v, "-") && len(v) > 1:
		cluster:
			for j := 1; j < len(v); j++ {
				c := v[j]
				switch {
				case c == 'm' && j == len(v)-1:
					if i+1 < len(args) {
						blank(args[i+1])
						i++
					}
				case c == 'm':
					blank(args[i])
					break cluster
				case strings.IndexByte(opts.flags, c) >= 0:
				case strings.IndexByte(opts.value, c) >= 0:
					if j == len(v)-1 {
						i++
					}
					break cluster
				case strings.IndexByte(opts.optional, c) >= 0:
					break cluster
				default:
					return
				}
			}
		}
	}
}

// assignment reports a NAME=value word.
func assignment(w string) bool {
	i := strings.IndexByte(w, '=')
	if i <= 0 {
		return false
	}
	for j, r := range w[:i] {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (j == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// shellToken is one word, redirection, or separator of a command, with its
// span in the text and, for a word, its value with quotes removed.
type shellToken struct {
	start, end int
	text       string
	value      string
	sep        bool // ; & | && || newline ( )
	redir      bool // > >> < >& <& >| &> &>> with any fd number before it
}

// shellTokens splits cmd into words, redirections, and separators, as a
// POSIX shell would for these purposes: quotes and backslashes group, and
// an unquoted ; & | ( ) newline separates. ok is false for an open quote.
func shellTokens(cmd string) (toks []shellToken, ok bool) {
	i := 0
	for i < len(cmd) {
		c := cmd[i]
		switch c {
		case ' ', '\t':
			i++
		case '\n', ';', '(', ')':
			toks = append(toks, shellToken{start: i, end: i + 1, text: string(c), sep: true})
			i++
		case '|', '&':
			if c == '&' && i+1 < len(cmd) && cmd[i+1] == '>' {
				j := i + 2
				if j < len(cmd) && cmd[j] == '>' {
					j++
				}
				toks = append(toks, shellToken{start: i, end: j, text: cmd[i:j], redir: true})
				i = j
				continue
			}
			j := i + 1
			if j < len(cmd) && cmd[j] == c {
				j++
			}
			toks = append(toks, shellToken{start: i, end: j, text: cmd[i:j], sep: true})
			i = j
		case '>', '<':
			j := i + 1
			for j < len(cmd) && strings.IndexByte(">&|", cmd[j]) >= 0 {
				j++
			}
			toks = append(toks, shellToken{start: i, end: j, text: cmd[i:j], redir: true})
			i = j
		default:
			start := i
			var v strings.Builder
		word:
			for i < len(cmd) {
				c := cmd[i]
				switch {
				case c == '\'':
					k := strings.IndexByte(cmd[i+1:], '\'')
					if k < 0 {
						return nil, false
					}
					v.WriteString(cmd[i+1 : i+1+k])
					i += k + 2
				case c == '"':
					i++
					for {
						if i >= len(cmd) {
							return nil, false
						}
						if cmd[i] == '\\' && i+1 < len(cmd) {
							v.WriteByte(cmd[i+1])
							i += 2
							continue
						}
						if cmd[i] == '"' {
							i++
							break
						}
						v.WriteByte(cmd[i])
						i++
					}
				case c == '\\':
					if i+1 < len(cmd) {
						v.WriteByte(cmd[i+1])
					}
					i += 2
				case strings.IndexByte(" \t\n;&|()<>", c) >= 0:
					break word
				default:
					v.WriteByte(c)
					i++
				}
			}
			if i > len(cmd) {
				i = len(cmd)
			}
			// A word of digits right before a redirection is its fd number.
			if i < len(cmd) && (cmd[i] == '>' || cmd[i] == '<') && allDigits(cmd[start:i]) {
				j := i + 1
				for j < len(cmd) && strings.IndexByte(">&|", cmd[j]) >= 0 {
					j++
				}
				toks = append(toks, shellToken{start: start, end: j, text: cmd[start:j], redir: true})
				i = j
				continue
			}
			toks = append(toks, shellToken{start: start, end: i, text: cmd[start:i], value: v.String()})
		}
	}
	return toks, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
