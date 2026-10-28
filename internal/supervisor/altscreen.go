package supervisor

// altTracker follows whether the CLI has switched the terminal to its
// alternate screen (CSI ? 1049, 1047, or 47, then h or l), across reads.
// It only watches the output; it never changes it. The picker needs it
// because the alternate screen does not nest.
type altTracker struct {
	on     bool
	state  int // 0 ground, 1 ESC, 2 ESC [, 3 ESC [ ? params
	params []byte
}

// maxParams bounds the parameters kept for one sequence.
const maxParams = 32

func (a *altTracker) feed(b []byte) {
	for _, c := range b {
		switch a.state {
		case 0:
			if c == esc {
				a.state = 1
			}
		case 1:
			switch c {
			case '[':
				a.state = 2
			case 'c': // RIS, a full reset, leaves the alternate screen
				a.on, a.state = false, 0
			case esc:
			default:
				a.state = 0
			}
		case 2:
			switch c {
			case '?':
				a.state, a.params = 3, a.params[:0]
			case esc:
				a.state = 1
			default:
				a.state = 0
			}
		case 3:
			switch {
			case c >= '0' && c <= '9' || c == ';':
				if len(a.params) < maxParams {
					a.params = append(a.params, c)
				}
			case c == 'h' || c == 'l':
				a.apply(c == 'h')
				a.state = 0
			case c == esc:
				a.state = 1
			default:
				a.state = 0
			}
		}
	}
}

// idle reports whether the tracker is between sequences, so on is not
// about to change with the next byte.
func (a *altTracker) idle() bool { return a.state == 0 }

func (a *altTracker) apply(set bool) {
	start := 0
	for i := 0; i <= len(a.params); i++ {
		if i == len(a.params) || a.params[i] == ';' {
			switch string(a.params[start:i]) {
			case "1049", "1047", "47":
				a.on = set
			}
			start = i + 1
		}
	}
}
