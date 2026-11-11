package policyhook

import "strings"

// stripMessages returns cmd with the text of its message arguments blanked:
// the message of git commit or git tag (-m, --message, -m<msg>, -am <msg>,
// --message=<msg>) and the arguments of an echo or printf whose output is
// not piped. They are text the command prints or stores, not a path it
// opens, so naming aggy's index there is no reason to ask (T6954). It is
// conservative: a command with a substitution ($(, `, <(, >(), a here
// document, or anything it cannot lex (an open quote) comes back whole, and
// a redirection's target is never blanked.
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
	for start := 0; start < len(toks); {
		end := start
		for end < len(toks) && !toks[end].sep {
			end++
		}
		piped := end < len(toks) && toks[end].text == "|"
		stripSimple(toks[start:end], piped, blank)
		start = end + 1
	}
	return string(out)
}

// stripSimple blanks the message arguments of one simple command (words and
// redirections, no separators).
func stripSimple(toks []shellToken, piped bool, blank func(shellToken)) {
	// The words that are arguments: not a redirection, not its target, and
	// not a leading NAME=value.
	var args []shellToken
	target := false
	for _, t := range toks {
		switch {
		case t.redir:
			target = true // the next word is its target (or fd): never blanked
		case target:
			target = false
		case len(args) == 0 && assignment(t.value):
		default:
			args = append(args, t)
		}
	}
	if len(args) == 0 {
		return
	}
	switch args[0].value {
	case "echo", "printf":
		if piped {
			return
		}
		for _, a := range args[1:] {
			blank(a)
		}
	case "git":
		sub := -1
		for i, a := range args[1:] {
			if a.value == "commit" || a.value == "tag" {
				sub = i + 1
				break
			}
		}
		if sub < 0 {
			return
		}
		for i := sub + 1; i < len(args); i++ {
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
			case strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "--"):
				j := strings.IndexByte(v, 'm')
				switch {
				case j < 0:
				case j == len(v)-1:
					if i+1 < len(args) {
						blank(args[i+1])
						i++
					}
				default:
					blank(args[i])
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
