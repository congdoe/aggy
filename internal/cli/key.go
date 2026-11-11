package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/keyring"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// aggy key (T6931, docs/design.md section 15, "Keys"): API keys for an open
// harness, in the OS keychain. aggy's index keeps a key's name and provider,
// never its value, and no command prints a value: there is no show, get, or
// export. A key goes only to OpenCode, at a launch a person started at a
// terminal (prepareOpenCode), as a 0600 file in that launch's own folder.

const keyUsage = `Usage: aggy key <command>

API keys for OpenCode, kept in your OS keychain: the macOS Keychain or the Linux
secret service. aggy's index keeps each key's name and the OpenCode provider it
is for, never the value, and no aggy command prints a value.

  add NAME [--provider P]   Store a key. The keychain tool (security, or
                            secret-tool) asks for it on this terminal with echo
                            off, so it never passes through aggy, an argument,
                            or an environment variable. P is the OpenCode
                            provider it is for (default: NAME), one key each;
                            claude-agent and opencode are refused.
  list                      Each key's name, its provider, and whether its item
                            is in the keychain (macOS; not checked on Linux,
                            where checking would read the value)
  remove NAME [--provider P]
                            Delete a key from the keychain and aggy's index
                            (--provider only for an item the index no longer
                            lists: the item is stored as NAME@P)

A key goes only to OpenCode, at a launch you start at a terminal, for a provider
the repo's allowed_vendors lets in, as a 0600 file in that launch's own folder,
removed when OpenCode exits. Any process OpenCode starts can read it. It never
reaches Claude Code, Codex, Grok, Hermes, or a borrow, and a repo cannot name one.
On macOS every read asks you in a dialog: do not choose Always Allow there, or
any program could read the key through security without asking.
add and remove ask you at a terminal; a session's tool shell is refused, which
holds against mistakes, not a model that tries. aggy unwire leaves keys alone.
`

// keychainHelperPath is set only by a test build (-ldflags -X), to put the fake
// keychain tool in place of the real one; empty, the fixed path for this
// system is used (keyring.HelperFor).
var keychainHelperPath = ""

// keyHelper is the keychain tool on this system, checked to be there. A test
// replaces it.
var keyHelper = func() (keyring.Helper, error) {
	h, err := keyring.HelperFor(runtime.GOOS)
	if err != nil {
		return h, err
	}
	if keychainHelperPath != "" {
		h.Path = keychainHelperPath
	}
	if _, err := safefs.Stat(h.Path); err != nil {
		return h, fmt.Errorf("%s is not at %s, so aggy cannot reach %s; aggy never keeps a key in a file", h.Name(), h.Path, h.Store())
	}
	return h, nil
}

// keyTerminal is the terminal the keychain tool asks on; a test replaces it.
var keyTerminal = func() *os.File { return os.Stdin }

// How long each keychain step may take: typing a key, answering macOS's
// dialog, and the rest.
const (
	keyPromptTimeout = 5 * time.Minute
	keyReadTimeout   = 2 * time.Minute
	keyRunTimeout    = 30 * time.Second
)

func runKey(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, keyUsage)
		return 0
	}
	switch args[0] {
	case "add":
		return keyAdd(args[1:], stdout, stderr)
	case "list":
		if len(args) > 1 {
			return cmdUsage(stderr, "key list", "takes no arguments", keyUsage)
		}
		return keyList(stdout, stderr)
	case "remove":
		return keyRemove(args[1:], stdout, stderr)
	}
	// Not quoted: a key typed in the wrong place is not printed back.
	return cmdUsage(stderr, "key", "unknown command: use add, list, or remove (no command prints a key)", keyUsage)
}

// keyRefuse prints and records a refusal of aggy key's: the record holds the
// code's text, never the key's name or the message.
func keyRefuse(stderr io.Writer, cmd string, code diag.Code, msg string) int {
	refuse(context.Background(), stderr, cmd, diag.EvKeyRefused, code, msg, diag.Rec{})
	return 1
}

// keyGate refuses a key change anyone but a person at a terminal asks for:
// personGate, and stderr a terminal too, as the in-place questions need.
func keyGate(getenv func(string) string) error {
	if err := personGate(getenv); err != nil {
		return err
	}
	if !offerTerminal() {
		return errors.New("this asks you at a terminal, and stdin or stderr is not one")
	}
	return nil
}

func keyAdd(args []string, stdout, stderr io.Writer) int {
	const name = "key add"
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	provider := fl.String("provider", "", "")
	var pos []string
	for rest := args; ; {
		if err := fl.Parse(rest); err != nil {
			// flag's own text quotes the word; a key typed as a flag is not
			// printed back.
			return cmdUsage(stderr, name, "unknown flag or a flag with no value: aggy key add takes --provider P only", keyUsage)
		}
		if fl.NArg() == 0 {
			break
		}
		pos = append(pos, fl.Arg(0))
		rest = fl.Args()[1:]
	}
	if len(pos) != 1 {
		return cmdUsage(stderr, name, "want one key name, and never the key itself: the keychain tool asks for it", keyUsage)
	}
	key := pos[0]
	if err := keyring.CheckName(key); err != nil {
		return keyRefuse(stderr, name, diag.CodeKeyBadName, err.Error())
	}
	if *provider == "" {
		*provider = key
	}
	if err := keyring.CheckProvider(*provider); err != nil {
		return keyRefuse(stderr, name, diag.CodeKeyBadName, err.Error())
	}
	if err := keyGate(os.Getenv); err != nil {
		return keyRefuse(stderr, name, diag.CodeNotAtKeyboard, err.Error())
	}
	h, err := keyHelper()
	if err != nil {
		return keyRefuse(stderr, name, diag.CodeKeyUnsupported, err.Error())
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return keyRefuse(stderr, name, diag.CodeStore, err.Error())
	}
	defer func() { _ = s.Close() }()
	keys, err := s.Keys(ctx)
	if err != nil {
		return keyRefuse(stderr, name, diag.CodeStore, err.Error())
	}
	if i := slices.IndexFunc(keys, func(k store.Key) bool { return k.Provider == *provider && k.Name != key }); i >= 0 {
		return keyRefuse(stderr, name, diag.CodeKeyProviderTaken, fmt.Sprintf("key %s is already bound to provider %s, and a provider takes one key: aggy key remove %s first", keys[i].Name, *provider, keys[i].Name))
	}
	if i := slices.IndexFunc(keys, func(k store.Key) bool { return k.Name == key && k.Provider != *provider }); i >= 0 {
		// The provider is part of the keychain item (keyring.Account), so a
		// new provider is a new item: the old one goes first.
		return keyRefuse(stderr, name, diag.CodeKeyProviderTaken, fmt.Sprintf("key %s is bound to provider %s: aggy key remove %s first, or pick another name", key, keys[i].Provider, key))
	}
	if i := slices.IndexFunc(keys, func(k store.Key) bool { return k.Name == key }); i >= 0 {
		yes, _ := askInPlace(os.Getenv, offerIn, stdout, inPlace{
			change: fmt.Sprintf("A key named %s is already stored, for provider %s. Adding it again replaces it in %s.", key, keys[i].Provider, h.Store()),
			ask:    "Replace it?",
		})
		if !yes {
			fmt.Fprintln(stdout, "Nothing changed.")
			return 1
		}
	}
	fmt.Fprintf(stdout, "%s asks for the key now. Paste or type it; it is not shown, and it goes from this terminal to %s without passing through aggy.\n", h.Name(), h.Store())
	if h.Kind == keyring.Security {
		fmt.Fprintln(stdout, "The item trusts no program, so each time aggy reads it for an OpenCode launch, macOS asks you in a dialog. Do not choose Always Allow there: any program could then read the key through security without asking.")
	}
	code, err := execrun.KeychainPrompt(ctx, h.Path, h.AddArgs(keyring.Account(key, *provider)), keyTerminal(), keyPromptTimeout)
	if err != nil {
		return keyRefuse(stderr, name, diag.CodeKeyHelperFailed, fmt.Sprintf("%s did not finish (%s), so nothing was stored", h.Name(), keyErrWord(err)))
	}
	if code != 0 {
		return keyRefuse(stderr, name, diag.CodeKeyHelperFailed, fmt.Sprintf("%s exited %d, so nothing was stored", h.Name(), code))
	}
	if err := s.PutKey(ctx, store.Key{Name: key, Provider: *provider, AddedAt: time.Now()}); err != nil {
		return keyRefuse(stderr, name, diag.CodeStore, fmt.Sprintf("the key is in %s, but aggy could not record its name: %v. aggy key add %s --provider %s again records it", h.Store(), err, key, *provider))
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvKeyAdded, Msg: "key stored", Attrs: map[string]any{"provider": *provider}})
	fmt.Fprintf(stdout, "Stored key %s for OpenCode's provider %s in %s. OpenCode gets it at the next launch you start at a terminal where that provider is allowed; Claude Code, Codex, and Grok never do. aggy key remove %s deletes it.\n", key, *provider, h.Store(), key)
	return 0
}

// keyErrWord is a fixed word for err: aggy never shows the keychain tool's own
// text, nor an error that could quote it.
func keyErrWord(err error) string {
	if errors.Is(err, execrun.ErrTimeout) {
		return "it timed out"
	}
	return "it could not run"
}

func keyList(stdout, stderr io.Writer) int {
	const name = "key list"
	ctx := context.Background()
	path, err := store.DefaultPath()
	if err != nil {
		return keyRefuse(stderr, name, diag.CodeStore, err.Error())
	}
	var keys []store.Key
	if _, err := safefs.Stat(path); err == nil {
		s, err := store.OpenReadOnly(ctx, path)
		if err != nil {
			return keyRefuse(stderr, name, diag.CodeStore, err.Error())
		}
		keys, err = s.Keys(ctx)
		_ = s.Close()
		if err != nil {
			return keyRefuse(stderr, name, diag.CodeStore, err.Error())
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return keyRefuse(stderr, name, diag.CodeStore, err.Error())
	}
	if len(keys) == 0 {
		fmt.Fprintln(stdout, "No keys. aggy key add NAME --provider P stores one.")
		return 0
	}
	h, herr := keyHelper()
	fmt.Fprintf(stdout, "%-20s %-20s %s\n", "NAME", "PROVIDER", "KEYCHAIN")
	for _, k := range keys {
		fmt.Fprintf(stdout, "%-20s %-20s %s\n", termsafe.Field(k.Name), termsafe.Field(k.Provider), keyPresence(ctx, h, herr, keyring.Account(k.Name, k.Provider)))
	}
	return 0
}

// keyPresence says whether the keychain holds the key, without reading it.
func keyPresence(ctx context.Context, h keyring.Helper, herr error, account string) string {
	if herr != nil {
		return "unknown: no keychain tool"
	}
	args, ok := h.PresenceArgs(account)
	if !ok {
		return "not checked"
	}
	code, err := execrun.KeychainRun(ctx, h.Path, args, keyRunTimeout)
	switch {
	case err != nil:
		return "unknown"
	case code == 0:
		return "present"
	case h.NotFound(code):
		return "missing"
	}
	return "unknown"
}

func keyRemove(args []string, stdout, stderr io.Writer) int {
	const name = "key remove"
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	provider := fl.String("provider", "", "")
	var pos []string
	for rest := args; ; {
		if err := fl.Parse(rest); err != nil {
			return cmdUsage(stderr, name, "unknown flag or a flag with no value: aggy key remove takes --provider P only", keyUsage)
		}
		if fl.NArg() == 0 {
			break
		}
		pos = append(pos, fl.Arg(0))
		rest = fl.Args()[1:]
	}
	if len(pos) != 1 {
		return cmdUsage(stderr, name, "want one key name", keyUsage)
	}
	key := pos[0]
	if err := keyring.CheckName(key); err != nil {
		return keyRefuse(stderr, name, diag.CodeKeyBadName, err.Error())
	}
	if *provider != "" {
		if err := keyring.CheckProvider(*provider); err != nil {
			return keyRefuse(stderr, name, diag.CodeKeyBadName, err.Error())
		}
	}
	if err := keyGate(os.Getenv); err != nil {
		return keyRefuse(stderr, name, diag.CodeNotAtKeyboard, err.Error())
	}
	h, err := keyHelper()
	if err != nil {
		return keyRefuse(stderr, name, diag.CodeKeyUnsupported, err.Error())
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return keyRefuse(stderr, name, diag.CodeStore, err.Error())
	}
	defer func() { _ = s.Close() }()
	k, known, err := s.KeyNamed(ctx, key)
	if err != nil {
		return keyRefuse(stderr, name, diag.CodeStore, err.Error())
	}
	switch {
	case known && *provider != "" && *provider != k.Provider:
		return keyRefuse(stderr, name, diag.CodeKeyBadName, fmt.Sprintf("key %s is bound to provider %s, not %s", key, k.Provider, *provider))
	case known:
		*provider = k.Provider
	case *provider == "":
		// The item's account names the provider (keyring.Account).
		return keyRefuse(stderr, name, diag.CodeKeyBadName, fmt.Sprintf("aggy's index does not list key %s; aggy key remove %s --provider P deletes the keychain item it would have for provider P", key, key))
	}
	what := "It is bound to OpenCode's provider " + *provider + "."
	if !known {
		what = "aggy's index does not list it, so aggy deletes the keychain item for provider " + *provider + ", if there is one."
	}
	yes, _ := askInPlace(os.Getenv, offerIn, stdout, inPlace{
		change: fmt.Sprintf("This deletes key %s from %s and aggy's index. %s", key, h.Store(), what),
		ask:    "Delete it?",
	})
	if !yes {
		fmt.Fprintln(stdout, "Nothing changed.")
		return 1
	}
	code, err := execrun.KeychainRun(ctx, h.Path, h.RemoveArgs(keyring.Account(key, *provider)), keyRunTimeout)
	if err != nil {
		return keyRefuse(stderr, name, diag.CodeKeyHelperFailed, fmt.Sprintf("%s did not finish (%s), so the key is still there", h.Name(), keyErrWord(err)))
	}
	gone := h.NotFound(code)
	if code != 0 && !gone {
		return keyRefuse(stderr, name, diag.CodeKeyHelperFailed, fmt.Sprintf("%s exited %d, so the key is still there", h.Name(), code))
	}
	if err := s.DeleteKey(ctx, key); err != nil {
		return keyRefuse(stderr, name, diag.CodeStore, err.Error())
	}
	attrs := map[string]any{}
	if known {
		attrs["provider"] = k.Provider
	}
	dlog.Log(ctx, diag.Rec{Event: diag.EvKeyRemoved, Msg: "key removed", Attrs: attrs})
	switch {
	case gone && !known:
		fmt.Fprintf(stdout, "No key named %s: nothing was in %s or aggy's index.\n", key, h.Store())
	case gone:
		fmt.Fprintf(stdout, "Key %s was not in %s; aggy's index no longer lists it.\n", key, h.Store())
	default:
		fmt.Fprintf(stdout, "Deleted key %s from %s and aggy's index.\n", key, h.Store())
	}
	return 0
}

// openCodeKey is one stored key a launch read for OpenCode.
type openCodeKey struct {
	name, provider string
	value          *keyring.Value
}

// readKeychain reads key k for a launch, at that moment only, from the item
// bound to its name and provider (keyring.Account). The error never quotes the
// tool's text or the value.
func readKeychain(ctx context.Context, k store.Key) (*keyring.Value, error) {
	h, err := keyHelper()
	if err != nil {
		return nil, err
	}
	v, code, err := execrun.KeychainRead(ctx, h.Path, h.ReadArgs(keyring.Account(k.Name, k.Provider)), keyReadTimeout)
	switch {
	case err != nil && code == 0:
		return nil, err // the value's own check (keyring.Parse), or the tool did not run
	case err != nil:
		return nil, fmt.Errorf("%s did not finish (%s)", h.Name(), keyErrWord(err))
	case h.NotFound(code):
		return nil, fmt.Errorf("it is not in %s", h.Store())
	case code != 0:
		return nil, fmt.Errorf("%s exited %d (on macOS, Deny in its dialog)", h.Name(), code)
	}
	return v, nil
}

// openCodeKeys reads the stored keys this launch may hand OpenCode: each bound
// to a provider the repo's allowed_vendors lets in, read only when a person
// started the launch at a terminal (person), so a model's shell cannot get a
// key handed to an OpenCode it runs. It returns the keys read, which the
// caller must wipe, and the launch's lines about them.
func openCodeKeys(in openCodeInput) (got []openCodeKey, warns []string) {
	if len(in.keys) == 0 {
		return nil, nil
	}
	if !in.person || in.readKey == nil {
		for _, k := range in.keys {
			keyNotPassed(k.Provider)
		}
		return nil, []string{"your stored keys are not handed to OpenCode here: aggy hands a key only to a launch a person starts at a terminal, outside a session's tool shell"}
	}
	for _, k := range in.keys {
		// The index is a file a model's shell can write: a row is checked
		// again, so one cannot bind a key to claude-agent or to OpenCode's own
		// provider, which only opencode-free-models turns on.
		if keyring.CheckName(k.Name) != nil || keyring.CheckProvider(k.Provider) != nil {
			keyNotPassed(k.Provider)
			warns = append(warns, "a key in aggy's index has a name or provider aggy key never stores, so it is not handed to OpenCode; aggy key list shows it")
			continue
		}
		if !in.cfg.OpenCodeProviderAllowed(k.Provider) {
			keyNotPassed(k.Provider)
			warns = append(warns, fmt.Sprintf("key %s is not handed to OpenCode: this repo's allowed_vendors does not list %s", k.Name, k.Provider))
			continue
		}
		v, err := in.readKey(k)
		if err != nil {
			keyNotPassed(k.Provider)
			warns = append(warns, fmt.Sprintf("key %s is not handed to OpenCode, so %s is not offered: %v", k.Name, k.Provider, err))
			continue
		}
		got = append(got, openCodeKey{name: k.Name, provider: k.Provider, value: v})
	}
	return got, warns
}

func keyNotPassed(provider string) {
	dlog.Log(context.Background(), diag.Rec{Event: diag.EvKeyNotPassed, Code: diag.CodeKeyNotPassed, Msg: fixedMsg(diag.CodeKeyNotPassed),
		Vendor: "opencode", Attrs: map[string]any{"provider": provider}})
}

// Doctor's key findings' codes.
const (
	codeKeysStored   = "keys-stored"
	codeKeysNoHelper = "keys-no-keychain-tool"
)

// keyFindings are aggy doctor's lines about aggy key: how many keys the index
// lists, and whether the keychain tool is there. They name no key (a name is
// the person's own, and --report is pasted publicly) and read no value. The
// index is opened read-only, and a missing one is no keys.
func keyFindings(ctx context.Context) []checkup.Finding {
	path, err := store.DefaultPath()
	if err != nil {
		return nil
	}
	if _, err := safefs.Stat(path); err != nil {
		return nil
	}
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return nil
	}
	keys, err := s.Keys(ctx)
	_ = s.Close()
	if err != nil || len(keys) == 0 {
		return nil
	}
	out := []checkup.Finding{{Code: codeKeysStored, Level: checkup.LevelInfo, Vendor: adapter.OpenCode,
		Message: fmt.Sprintf("%d API key(s) stored with aggy key, handed only to OpenCode at a launch you start (aggy key list names them)", len(keys))}}
	if _, err := keyHelper(); err != nil {
		out = append(out, checkup.Finding{Code: codeKeysNoHelper, Level: checkup.LevelWarn, Vendor: adapter.OpenCode,
			Message: "aggy key lists keys, but the OS keychain tool is not there, so no key reaches OpenCode", Detail: err.Error()})
	}
	return out
}

// storedKeys are the keys aggy key recorded in the index; none when it cannot
// be read, which a launch treats as no keys.
func storedKeys(ctx context.Context, s *store.Store) []store.Key {
	keys, err := s.Keys(ctx)
	if err != nil {
		return nil
	}
	return keys
}

// repoKeysLine is the launch's line for a repo's [keys] table, which aggy
// ignores: a repo can never name a key or bind one to a provider.
func repoKeysLine(rc config.RepoConfig) string {
	if !rc.SetsKeys() {
		return ""
	}
	return "this repo's .aggy/config.toml has a [keys] table, which aggy ignores: only you store and bind keys, with aggy key"
}

// keysLeftNote is what aggy unwire --all says about keys: it never deletes a
// secret on its own, so it names each key and the command that removes it.
func keysLeftNote(ctx context.Context, s *store.Store) string {
	keys, err := s.Keys(ctx)
	if err != nil || len(keys) == 0 {
		return ""
	}
	var names []string
	for _, k := range keys {
		names = append(names, termsafe.Field(k.Name))
	}
	return fmt.Sprintf("aggy unwire leaves your stored keys in the OS keychain: %s. aggy key remove <name> deletes one.", strings.Join(names, ", "))
}
