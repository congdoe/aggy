// Package keyring is the pure half of aggy key (T6931, docs/design.md
// section 15, "Keys"): key names and their check, the OS tool that holds the
// keys and the argument lists aggy runs it with, and Value, which holds a
// key read back for one launch. It starts nothing and reads nothing:
// internal/execrun runs the tool, and internal/cli decides when.
//
// A key's value never appears in an argument list built here: none of these
// functions takes one. On macOS the security tool asks for it itself, on the
// person's terminal (-w given last, with no value); on Linux secret-tool
// store reads it from its stdin, which aggy gives only as the person's
// terminal, where secret-tool asks for it itself. aggy never sees the value
// it stores.
package keyring

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
)

// Service is the one service name aggy stores and reads its keychain items
// under, with Account as the account. aggy reads no item under any other
// service, so a vendor's own token, or any other item, is never read.
const Service = "aggy-key"

// Account is the keychain account of the key called name, bound to provider:
// "<name>@<provider>". The provider is part of the item, not only of aggy's
// index, so an index row a model's shell rewrote to bind a key to another
// provider names an item that is not there, and the key never goes to that
// provider. Neither part can hold "@".
func Account(name, provider string) string { return name + "@" + provider }

// nameRE is a key name: short, lower case, no spaces, so it can never be
// read as a flag or an option by the tool.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// CheckName checks a key name. The error never quotes what it was given: a
// key typed where its name goes must not be printed back.
func CheckName(name string) error {
	if !nameRE.MatchString(name) {
		return errors.New("that is not a key name: use a lower-case letter, then up to 31 lower-case letters, digits, or dashes. Never give the key itself as an argument: the keychain tool asks for it")
	}
	return nil
}

// providerRE is an OpenCode provider id (config's own rule).
var providerRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// NeverProviders are the providers no key may be bound to: claude-agent runs
// the local claude, so a key there would reach Claude Code; opencode is
// OpenCode's own service, which only aggy config set opencode-free-models
// turns on (T6956).
var NeverProviders = []string{"claude-agent", "opencode"}

// CheckProvider checks the provider a key is bound to.
func CheckProvider(p string) error {
	switch {
	case !providerRE.MatchString(p):
		// Not quoted, as for a name.
		return errors.New("that is not an OpenCode provider id (lower case, digits, dots, dashes, underscores)")
	case slices.Contains(NeverProviders, p):
		return fmt.Errorf("aggy never binds a key to provider %q", p)
	}
	return nil
}

// Kind is which OS tool holds the keys.
type Kind int

// The tools.
const (
	// Security is macOS's /usr/bin/security, on the login keychain.
	Security Kind = iota + 1
	// SecretTool is libsecret's secret-tool, on the Linux secret service.
	SecretTool
)

// ErrUnsupported is a system with no keychain tool aggy uses. There is no
// fallback to a file.
var ErrUnsupported = errors.New("aggy key stores keys only in the macOS Keychain or the Linux secret service, and this system has neither")

// Helper is the OS tool and where it is. The path is fixed per system, never
// looked up on PATH, so nothing on PATH can stand in for it.
type Helper struct {
	Kind Kind
	Path string
}

// helperPaths are the fixed paths.
var helperPaths = map[string]Helper{
	"darwin": {Security, "/usr/bin/security"},
	"linux":  {SecretTool, "/usr/bin/secret-tool"},
}

// HelperFor is goos's tool, or ErrUnsupported.
func HelperFor(goos string) (Helper, error) {
	h, ok := helperPaths[goos]
	if !ok {
		return Helper{}, ErrUnsupported
	}
	return h, nil
}

// Name is the tool's name for messages.
func (h Helper) Name() string {
	if h.Kind == Security {
		return "security"
	}
	return "secret-tool"
}

// Store is where the tool keeps keys, for messages.
func (h Helper) Store() string {
	if h.Kind == Security {
		return "your macOS login keychain"
	}
	return "your Linux secret service"
}

// label is the item's label, from its account: what the person sees in Keychain Access or
// Seahorse.
func label(account string) string { return "aggy key " + account }

// AddArgs store a key under account (Account). With security, -T "" leaves the item's list
// of trusted programs empty, so every read, by security itself too, asks the
// person in a macOS dialog (unverified live); -U replaces an item already
// there; and -w is last with no value, so security asks for the value on the
// terminal (man security, add-generic-password: "-w password  Specify
// password to be added. Put at end of command to be prompted (recommended)";
// unverified live). secret-tool store reads the value from stdin, and asks on
// the terminal when stdin is one (secret-tool(1); unverified live).
func (h Helper) AddArgs(account string) []string {
	if h.Kind == Security {
		return []string{"add-generic-password", "-U", "-s", Service, "-a", account, "-l", label(account), "-T", "", "-w"}
	}
	return []string{"store", "--label=" + label(account), "service", Service, "account", account}
}

// ReadArgs print the key under account on stdout.
func (h Helper) ReadArgs(account string) []string {
	if h.Kind == Security {
		return []string{"find-generic-password", "-s", Service, "-a", account, "-w"}
	}
	return []string{"lookup", "service", Service, "account", account}
}

// PresenceArgs find the item without reading its value: security without -w
// or -g prints only the item's attributes. ok is false where the tool has no
// such form: secret-tool search prints the value too, so on Linux aggy does
// not check.
func (h Helper) PresenceArgs(account string) (args []string, ok bool) {
	if h.Kind == Security {
		return []string{"find-generic-password", "-s", Service, "-a", account}, true
	}
	return nil, false
}

// RemoveArgs delete the item under account.
func (h Helper) RemoveArgs(account string) []string {
	if h.Kind == Security {
		return []string{"delete-generic-password", "-s", Service, "-a", account}
	}
	return []string{"clear", "service", Service, "account", account}
}

// securityNotFound is security's exit status for an item that is not there
// (errSecItemNotFound; unverified live).
const securityNotFound = 44

// NotFound reports whether exit status code from the tool means the item is
// not there. secret-tool gives no status of its own for that, so it is never
// read as not found.
func (h Helper) NotFound(code int) bool { return h.Kind == Security && code == securityNotFound }

// MaxValue caps a key read back: longer is refused.
const MaxValue = 4096

// Value is a key read back for one launch. Every way fmt, encoding/json,
// encoding, and log/slog have to print it prints a placeholder, so a %v of a
// struct holding one, a log line, or a JSON dump shows "[key]". Wipe zeroes
// its bytes once they are written where the launch needs them.
//
// What Go cannot promise: the runtime may have copied the bytes (a pipe's
// read, a string conversion, the garbage collector moving nothing but freeing
// late), and a core dump of aggy holds whatever memory was live. aggy makes
// no string of a value, and Go writes no core dump unless GOTRACEBACK=crash
// and the system allows one.
type Value struct{ b []byte }

// placeholder is what every printing of a Value shows.
const placeholder = "[key]"

// Parse takes raw, the tool's stdout, as a Value: one trailing newline (or
// CRLF) is dropped, and the rest must be 1 to MaxValue printable ASCII
// characters with no space. raw is the Value's backing array: the caller
// keeps no other reference. On error raw is zeroed, and the error never
// quotes it.
func Parse(raw []byte) (*Value, error) {
	b := raw
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n := len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	bad := len(b) == 0 || len(b) > MaxValue
	for _, c := range b {
		if c < 0x21 || c > 0x7e {
			bad = true
			break
		}
	}
	if bad {
		clear(raw)
		return nil, errors.New("the stored key is empty, too long, or holds a space or a character that is not printable ASCII")
	}
	return &Value{b: b}, nil
}

// Bytes are the value. The caller must not keep them past Wipe.
func (v *Value) Bytes() []byte {
	if v == nil {
		return nil
	}
	return v.b
}

// Wipe zeroes the value's bytes and drops them.
func (v *Value) Wipe() {
	if v == nil {
		return
	}
	clear(v.b)
	v.b = nil
}

// String prints the placeholder.
func (Value) String() string { return placeholder }

// GoString prints the placeholder, for %#v.
func (Value) GoString() string { return placeholder }

// Format prints the placeholder for every verb and flag.
func (Value) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(placeholder)) }

// MarshalJSON prints the placeholder as a JSON string.
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + placeholder + `"`), nil }

// LogValue prints the placeholder in a log/slog record.
func (Value) LogValue() slog.Value { return slog.StringValue(placeholder) }

// MarshalText prints the placeholder.
func (Value) MarshalText() ([]byte, error) { return []byte(placeholder), nil }
