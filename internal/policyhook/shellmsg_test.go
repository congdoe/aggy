package policyhook

import (
	"testing"

	"github.com/congdoe/aggy/internal/parity"
)

// TestProtectedStateFiles (T6954): aggy's index and diagnostic log ask when a
// command names them as a file, in every spelling the #134 review used, but
// not when only a commit message or an echo's text names them.
func TestProtectedStateFiles(t *testing.T) {
	protected := []string{"/home/u/.aggy"}
	for _, tt := range []struct {
		cmd  string
		want bool
	}{
		// The #134 review's spellings still ask.
		{`sqlite3 ~/.aggy/aggy.db "update hermes_launches set token=''"`, true},
		{`sqlite3 $HOME/.aggy/aggy.db .dump`, true},
		{`sqlite3 ${HOME}/.aggy/aggy.db .dump`, true},
		{`sqlite3 "$AGGY_HOME/aggy.db" .dump`, true},
		{`sqlite3 ${AGGY_HOME}/diag.db .dump`, true},
		{`sqlite3 /home/u/.aggy/aggy.db .dump`, true},
		{`cd ~/.aggy && sqlite3 aggy.db .dump`, true},
		{`cd $AGGY_HOME; sqlite3 aggy.db`, true},
		{`sqlite3 aggy.db .dump`, true},
		{`sqlite3 ./aggy.db`, true},
		{`cp x ${HOME}/.aggy/aggy.db-wal`, true},
		{`rm diag.db`, true},
		{`python3 -c "import sqlite3; sqlite3.connect('/home/u/.aggy/aggy.db')"`, true},
		{`python3 -c "import os,sqlite3; sqlite3.connect(os.path.expanduser('~/.aggy/aggy.db'))"`, true},
		{`python3 -c "import sqlite3; sqlite3.connect('aggy.db')"`, true},
		{`sqlite3 ag"gy".db`, true},
		{"sqlite3 \\\naggy.db", true},
		// A message's text that is also run, or reaches a file, still asks.
		{`echo "drop table x" > aggy.db`, true},
		{`echo x 2>diag.db`, true},
		{`echo .dump | sqlite3 aggy.db`, true},
		{`echo "sqlite3 aggy.db .dump" | sh`, true},
		{`git commit -m "$(sqlite3 aggy.db .dump)"`, true},
		{"git commit -m \"`cat diag.db`\"", true},
		{`git commit -m 'drop aggy.db' && sqlite3 aggy.db`, true},
		{`git commit -m "move ~/.aggy/aggy.db"`, true},
		{"git commit -F - <<EOF\ndrop aggy.db\nEOF", true},
		{`git commit -m 'unterminated aggy.db`, true},
		{`git commit aggy.db -m 'x'`, true},
		{`eval echo aggy.db`, true},
		// Ordinary text that only names them does not.
		{`git commit -m 'drop aggy.db cache'`, false},
		{`git commit -s -m "store: drop the aggy.db cache"`, false},
		{`git commit -am "diag.db: rotate"`, false},
		{`git commit --message="aggy.db"`, false},
		{`git -C repo commit -q -m 'aggy.db' -m 'diag.db too'`, false},
		{`git tag -a v1 -m "aggy.db schema 9"`, false},
		{`echo "aggy.db is aggy's index"`, false},
		{`printf '%s\n' "diag.db" >&2`, false},
		{`grep -n myaggy.db notes.txt`, false},
	} {
		if got := protectedText(tt.cmd, protected, nil); got != tt.want {
			t.Errorf("protectedText(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
	// Through a vendor's hook too: a commit message no longer asks on Claude.
	p := plan()
	for cmd, want := range map[string]parity.Verdict{
		`git commit -m 'drop aggy.db cache'`: parity.VerdictNone,
		`sqlite3 ~/.aggy/aggy.db .dump`:      parity.VerdictAsk,
	} {
		if got := judgeP(t, p, payload(t, "Bash", map[string]any{"command": cmd}, nil)); got.Verdict != want {
			t.Errorf("%s: %+v, want %s", cmd, got, want)
		}
	}
}
