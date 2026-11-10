package probe

import (
	"slices"
	"testing"
)

// TestValueFlags (T6962): the options help shows with a required value, in
// commander's and clap's forms; an optional value is not one.
func TestValueFlags(t *testing.T) {
	help := `Usage: x [options] [prompt]

Options:
  -d, --debug [filter]          Enable debug mode
  --add-dir <directories...>    Additional directories
  -m, --model <MODEL>           The model
  -i, --image <FILE>...         Images
  --mcp-config=<configs>        MCP configs
  -r, --resume [<SESSION_ID>]   Resume
  --verbose                     Say more
  --max-turns <N>               Turns
Commands:
  mcp <cmd>                     Not an option
`
	want := []string{"--add-dir...", "-m", "--model", "-i...", "--image...", "--mcp-config", "--max-turns"}
	if got := ValueFlags([]byte(help)); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSwitches (PR #124 re-review): only an option help shows with no value
// at all is a switch; an optional value, a required one, a list, and an =
// form are not, and neither is a name shown elsewhere with a value.
func TestSwitches(t *testing.T) {
	help := `Usage: x [options] [prompt]

Options:
  -d, --debug [filter]          Enable debug mode
  --add-dir <directories...>    Additional directories
  -m, --model <MODEL>           The model
  -r, --resume [<SESSION_ID>]   Resume
  --mcp-config=<configs>        MCP configs
  -p, --print                   Print and exit
  --verbose                     Say more
  -c, --continue                Continue
  --name                        Shown bare here
  -n, --name <name>             and with a value here
Commands:
  mcp <cmd>                     Not an option
`
	want := []string{"-p", "--print", "--verbose", "-c", "--continue"}
	if got := Switches([]byte(help)); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
