package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
)

// FileName is the user config file inside the state directory.
const FileName = "config.toml"

// Config is the user config. Load fills unset keys with Default's values.
type Config struct {
	// DefaultVendor is the vendor `aggy` opens. Empty means the first
	// connected vendor in claude, codex, grok order.
	DefaultVendor adapter.Vendor `toml:"default_vendor"`
	Vendors       Vendors        `toml:"vendors"`
	Switch        Switch         `toml:"switch"`
	Routing       Routing        `toml:"routing"`
	Budgets       Budgets        `toml:"budgets"`
	Media         Media          `toml:"media"`
	Policy        Policy         `toml:"policy"`
	Notify        Notify         `toml:"notify"`
	Worktree      UserWorktree   `toml:"worktree"`
	GC            GC             `toml:"gc"`
	Log           Log            `toml:"log"`
}

// Log is the diagnostic log's retention and level (docs/logging.md). It
// is user config only: a repo's .aggy/config.toml has no such table.
type Log struct {
	// RetentionDays keeps a record this long; aggy gc prunes older ones.
	RetentionDays int `toml:"retention_days"`
	// MaxMB caps diag.db; aggy gc prunes the oldest records past it.
	MaxMB int `toml:"max_mb"`
	// Level is the lowest level stored: error, warn, info (the default),
	// or debug. AGGY_DEBUG=1 stores debug records too.
	Level string `toml:"level"`
}

// MaxLogMB bounds log.max_mb.
const MaxLogMB = 10240

// GC is how long aggy gc keeps what it may remove, in days. It is user
// config only: a repo's .aggy/config.toml has no such table, so a cloned
// repo cannot make aggy delete sooner.
type GC struct {
	// PendingWorktreeDays keeps an edit worktree nobody accepted or
	// rejected for this long after its borrow ended.
	PendingWorktreeDays int `toml:"pending_worktree_days"`
	// AcceptedWorktreeDays keeps an accepted edit's worktree and branch
	// (for aggy pr) this long after the accept.
	AcceptedWorktreeDays int `toml:"accepted_worktree_days"`
	// ClosedChildDays keeps a closed borrow child in the index this long
	// after it was last active.
	ClosedChildDays int `toml:"closed_child_days"`
	// InboxDays keeps an inbox report this long after it was written.
	InboxDays int `toml:"inbox_days"`
}

// MaxGCDays bounds a gc retention setting (ten years).
const MaxGCDays = 3650

// UserWorktree holds the person's opt-ins for edit worktrees. It is user
// config only: a repo's .aggy/config.toml has no allow_setup key, so a
// cloned repo can name a setup command but never allow it.
type UserWorktree struct {
	// AllowSetup lists the setup commands aggy may run in a new worktree,
	// each for one repo and that exact command: a repo whose command
	// changes (a pull) needs the person's say-so again.
	AllowSetup []SetupApproval `toml:"allow_setup"`
}

// SetupApproval allows one repo's worktree setup command.
type SetupApproval struct {
	Repo    string  `toml:"repo"` // the main checkout, absolute
	Command Command `toml:"command"`
}

// AllowsSetup reports whether cmd is allowed as the setup command of the
// repo whose canonical path is repo; canon canonicalizes a path.
func (w UserWorktree) AllowsSetup(repo string, cmd Command, canon func(string) string) bool {
	for _, a := range w.AllowSetup {
		if canon(a.Repo) == repo && slices.Equal(a.Command, cmd) {
			return true
		}
	}
	return false
}

// Notify holds notification settings. It is user config only: a repo's
// .aggy/config.toml can never turn notifications on.
type Notify struct {
	// Desktop shows a desktop notification when the CLI on screen waits
	// for the person, a borrow finishes, or a vendor hits its limit
	// (internal/notify). Off by default.
	Desktop bool `toml:"desktop"`
}

// Media holds opt-ins for media tools. It is user config only: a repo's
// .aggy/config.toml can never turn a tool on.
type Media struct {
	// GrokFileTools turns on Grok's image_edit (image --edit),
	// image_to_video (every video borrow), and reference_to_video. Each
	// reads a local file at any absolute path the model names and uploads
	// it to xAI, so a brief that names a private image, or text the child
	// is steered by, can send that image off the machine. Off by default.
	GrokFileTools bool `toml:"grok_file_tools"`
}

// Policy configures aggy's safety policy. It is user config only: a
// repo's .aggy/config.toml has no such table.
type Policy struct {
	// Required makes aggy run refuse to start a CLI the policy cannot
	// reach (a flag missing from its help, an unwired Codex rules file).
	// False starts it anyway, with a warning. Default true.
	Required bool `toml:"required"`
}

// Vendors holds per-vendor settings.
type Vendors struct {
	Claude VendorConfig `toml:"claude"`
	Codex  VendorConfig `toml:"codex"`
	Grok   VendorConfig `toml:"grok"`
}

// VendorConfig is one vendor's settings.
type VendorConfig struct {
	// Model is passed to the CLI unchanged. Empty passes no model flag, so
	// the vendor uses its own default.
	Model string `toml:"model"`
}

// For returns the settings of vendor v.
func (vs Vendors) For(v adapter.Vendor) VendorConfig {
	switch v {
	case adapter.Claude:
		return vs.Claude
	case adapter.Codex:
		return vs.Codex
	case adapter.Grok:
		return vs.Grok
	}
	return VendorConfig{}
}

// Switch configures the vendor switch picker.
type Switch struct {
	// Order is the picker's preference. It preselects the first vendor that
	// is connected, has quota left, and is not the one on screen.
	Order []adapter.Vendor `toml:"order"`
	// Key opens the picker, e.g. "ctrl+\\" (the default); pressed twice
	// quickly, it is sent to the CLI. See ParseKey.
	Key string `toml:"key"`
	// WarnAt is the share of a vendor's limit used, 0 to 1, at which aggy
	// suggests a handoff at a turn end, where the vendor writes what it
	// has left on this machine (T6699). Zero turns the warning off.
	WarnAt float64 `toml:"warn_at"`
}

// DefaultWarnAt is the default switch.warn_at.
const DefaultWarnAt = 0.9

// DefaultSwitchKey is the default switch.key: Ctrl+\, which none of the
// three CLIs is known to use.
const DefaultSwitchKey = `ctrl+\`

// ParseKey returns the control byte of a switch key named "ctrl+<k>",
// where k is a letter or one of \\ ] ^ _ (case-insensitive). Keys that
// terminals, line editing, or the CLIs already give a meaning are
// refused (see reservedKeys).
func ParseKey(s string) (byte, error) {
	k, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(s)), "ctrl+")
	if !ok || len(k) != 1 {
		return 0, fmt.Errorf("switch.key %q: want ctrl+ and one letter or one of \\ ] ^ _", s)
	}
	c := k[0]
	if why, ok := reservedKeys[c]; ok {
		return 0, fmt.Errorf("switch.key %q: Ctrl+%s is %s; pick another key", s, strings.ToUpper(k), why)
	}
	switch {
	case c >= 'a' && c <= 'z':
		return c - 'a' + 1, nil
	case c == '\\' || c == ']' || c == '^' || c == '_':
		return c & 0x1f, nil
	}
	return 0, fmt.Errorf("switch.key %q: want ctrl+ and one letter or one of \\ ] ^ _", s)
}

// reservedKeys are the Ctrl keys a switch key may not take, with what
// they already do.
var reservedKeys = map[byte]string{
	'c': "interrupt",
	'd': "end of input",
	'z': "suspend",
	'y': "delayed suspend",
	'h': "Backspace",
	'i': "Tab",
	'j': "Enter (line feed)",
	'm': "Enter",
	'[': "Esc, which starts every escape sequence",
	's': "flow control (XOFF)",
	'q': "flow control (XON)",
	'u': "line editing (kill line)",
	'w': "line editing (erase word)",
	'v': "line editing (literal next)",
	'r': "line editing (reprint)",
	'o': "line editing (discard output)",
}

// NotAuthor is the routing value meaning any connected vendor except the
// one that wrote the change.
const NotAuthor = "not-author"

// Route is one kind's routing: an ordered vendor list, or NotAuthor.
type Route struct {
	Vendors   []adapter.Vendor
	NotAuthor bool
}

// UnmarshalTOML accepts a list of vendor names or the string "not-author".
func (r *Route) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		if x != NotAuthor {
			return fmt.Errorf("want a vendor list or %q, got %q", NotAuthor, x)
		}
		*r = Route{NotAuthor: true}
		return nil
	case []any:
		route := Route{Vendors: []adapter.Vendor{}}
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("vendor list holds %T, want strings", e)
			}
			vendor, err := adapter.ParseVendor(s)
			if err != nil {
				return err
			}
			if !slices.Contains(route.Vendors, vendor) { // a repeat is dropped
				route.Vendors = append(route.Vendors, vendor)
			}
		}
		*r = route
		return nil
	}
	return fmt.Errorf("want a vendor list or %q, got %T", NotAuthor, v)
}

// Routing maps each borrow kind to its route. An empty vendor list turns
// the kind off. A vendor listed twice keeps its first place.
type Routing struct {
	Image    Route `toml:"image"`
	Video    Route `toml:"video"`
	Research Route `toml:"research"`
	News     Route `toml:"news"`
	Review   Route `toml:"review"`
	Edit     Route `toml:"edit"`
}

// For returns the route for kind k.
func (r Routing) For(k adapter.Kind) Route {
	switch k {
	case adapter.Image:
		return r.Image
	case adapter.Video:
		return r.Video
	case adapter.Research:
		return r.Research
	case adapter.News:
		return r.News
	case adapter.Review:
		return r.Review
	case adapter.Edit:
		return r.Edit
	}
	return Route{}
}

// Budget bounds. A day is far past any sane borrow, and the bound keeps
// Minutes * time.Minute from overflowing.
const (
	MaxMinutes = 24 * 60
	MaxTurns   = 1000
)

// Budget limits one borrow. aggy kills the process past Minutes.
type Budget struct {
	Minutes int `toml:"minutes"`
	// Turns caps agent turns where the CLI supports it. Zero means no cap.
	Turns int `toml:"turns"`
}

// Budgets holds a budget per borrow kind.
type Budgets struct {
	Image    Budget `toml:"image"`
	Video    Budget `toml:"video"`
	Research Budget `toml:"research"`
	News     Budget `toml:"news"`
	Review   Budget `toml:"review"`
	Edit     Budget `toml:"edit"`
}

// For returns the budget for kind k.
func (b Budgets) For(k adapter.Kind) Budget {
	switch k {
	case adapter.Image:
		return b.Image
	case adapter.Video:
		return b.Video
	case adapter.Research:
		return b.Research
	case adapter.News:
		return b.News
	case adapter.Review:
		return b.Review
	case adapter.Edit:
		return b.Edit
	}
	return Budget{}
}

func vendors(vs ...adapter.Vendor) Route { return Route{Vendors: vs} }

// Default returns the generic defaults. Routing follows each CLI's built-in
// tools, not benchmarks; budgets follow the build plan.
func Default() Config {
	return Config{
		Switch: Switch{Order: []adapter.Vendor{adapter.Claude, adapter.Codex, adapter.Grok}, Key: DefaultSwitchKey, WarnAt: DefaultWarnAt},
		Policy: Policy{Required: true},
		GC:     GC{PendingWorktreeDays: 30, AcceptedWorktreeDays: 30, ClosedChildDays: 30, InboxDays: 30},
		Log:    Log{RetentionDays: 14, MaxMB: 50, Level: "info"},
		Routing: Routing{
			// TODO(T6638): Codex is left out of news and research on
			// purpose. Its apply_patch tool cannot be turned off, and its
			// errors tell a right guess of a file line from a wrong one,
			// so a research child could confirm file contents
			// (docs/verification.md, 2026-10-28). The Codex adapter
			// refuses both kinds; a user who lists codex here gets that
			// refusal, named by the router.
			News:     vendors(adapter.Grok),                // web search built in, clean tool list
			Research: vendors(adapter.Grok),                // web search with sources, clean tool list
			Image:    vendors(adapter.Codex, adapter.Grok), // built-in image generation
			Video:    vendors(adapter.Grok),                // only Grok has video tools
			// "not-author" walks codex, grok, claude and skips the
			// parent's vendor, which wrote the change: a different model
			// catches different things.
			Review: Route{NotAuthor: true},
			// Grok is passed over until its writing mode is verified
			// (borrow.editGate).
			Edit: vendors(adapter.Codex, adapter.Grok),
		},
		Budgets: Budgets{
			News:     Budget{Minutes: 5, Turns: 15},
			Research: Budget{Minutes: 10, Turns: 30},
			Image:    Budget{Minutes: 5},
			Video:    Budget{Minutes: 10},
			Review:   Budget{Minutes: 15},
			Edit:     Budget{Minutes: 45},
		},
	}
}

// Path returns the user config path in the state directory
// ($AGGY_HOME or ~/.aggy).
func Path() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// LoadDefault loads the user config from Path.
func LoadDefault() (Config, error) {
	p, err := Path()
	if err != nil {
		return Config{}, err
	}
	return Load(p)
}

// MaxFileSize caps a config file. Config is small; a larger file is a
// mistake or an attack.
const MaxFileSize = 1 << 20

// Load reads the config at path over the defaults: a regular file of at
// most MaxFileSize, not a symlink, in a directory that is not a symlink
// either (safefs.ReadConfig). A missing file gives the defaults.
// Unknown keys are an error that names them.
func Load(path string) (Config, error) {
	b, err := safefs.ReadConfig(path, MaxFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	cfg, err := Parse(string(b))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes config text over the defaults and validates it.
func Parse(text string) (Config, error) {
	cfg := Default()
	md, err := toml.Decode(text, &cfg)
	if err != nil {
		return Config{}, err
	}
	if err := unknownKinds(md); err != nil {
		return Config{}, err
	}
	if err := unknownKeys(md); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks vendor names and budgets.
func (c Config) Validate() error {
	if c.DefaultVendor != "" {
		if _, err := adapter.ParseVendor(string(c.DefaultVendor)); err != nil {
			return fmt.Errorf("default_vendor: %w", err)
		}
	}
	if len(c.Switch.Order) == 0 {
		return errors.New("switch.order must list at least one vendor")
	}
	seen := map[adapter.Vendor]bool{}
	for _, v := range c.Switch.Order {
		if _, err := adapter.ParseVendor(string(v)); err != nil {
			return fmt.Errorf("switch.order: %w", err)
		}
		if seen[v] {
			return fmt.Errorf("switch.order: vendor %q listed twice", v)
		}
		seen[v] = true
	}
	if _, err := ParseKey(c.Switch.Key); err != nil {
		return err
	}
	if !(c.Switch.WarnAt >= 0 && c.Switch.WarnAt <= 1) { // NaN fails too
		return fmt.Errorf("switch.warn_at must be 0 (off) to 1, got %v", c.Switch.WarnAt)
	}
	for i, a := range c.Worktree.AllowSetup {
		if !filepath.IsAbs(a.Repo) || len(a.Command) == 0 || a.Command[0] == "" {
			return fmt.Errorf("worktree.allow_setup[%d]: want an absolute repo and a program with its arguments", i)
		}
	}
	for _, d := range []struct {
		key string
		n   int
	}{
		{"pending_worktree_days", c.GC.PendingWorktreeDays},
		{"accepted_worktree_days", c.GC.AcceptedWorktreeDays},
		{"closed_child_days", c.GC.ClosedChildDays},
		{"inbox_days", c.GC.InboxDays},
	} {
		if d.n < 1 || d.n > MaxGCDays {
			return fmt.Errorf("gc.%s must be 1 to %d, got %d", d.key, MaxGCDays, d.n)
		}
	}
	if c.Log.RetentionDays < 1 || c.Log.RetentionDays > MaxGCDays {
		return fmt.Errorf("log.retention_days must be 1 to %d, got %d", MaxGCDays, c.Log.RetentionDays)
	}
	if c.Log.MaxMB < 1 || c.Log.MaxMB > MaxLogMB {
		return fmt.Errorf("log.max_mb must be 1 to %d, got %d", MaxLogMB, c.Log.MaxMB)
	}
	if !slices.Contains([]string{"error", "warn", "info", "debug"}, c.Log.Level) {
		return fmt.Errorf("log.level must be error, warn, info, or debug, got %q", c.Log.Level)
	}
	for _, k := range adapter.Kinds() {
		r := c.Routing.For(k)
		if r.NotAuthor && k != adapter.Review {
			return fmt.Errorf("routing.%s: %q is only valid for review", k, NotAuthor)
		}
		for _, v := range r.Vendors {
			if _, err := adapter.ParseVendor(string(v)); err != nil {
				return fmt.Errorf("routing.%s: %w", k, err)
			}
		}
		b := c.Budgets.For(k)
		if b.Minutes < 1 || b.Minutes > MaxMinutes {
			return fmt.Errorf("budgets.%s.minutes must be 1 to %d, got %d", k, MaxMinutes, b.Minutes)
		}
		if b.Turns < 0 || b.Turns > MaxTurns {
			return fmt.Errorf("budgets.%s.turns must be 0 to %d, got %d", k, MaxTurns, b.Turns)
		}
	}
	return nil
}

// unknownKinds names an undecoded key under [routing] or [budgets] as an
// unknown borrow kind, which reads better than "unknown key".
func unknownKinds(md toml.MetaData) error {
	for _, k := range md.Undecoded() {
		if len(k) < 2 || (k[0] != "routing" && k[0] != "budgets") {
			continue
		}
		if _, err := adapter.ParseKind(k[1]); err != nil {
			return fmt.Errorf("%s.%s: %w", k[0], k[1], err)
		}
	}
	return nil
}

// unknownKeys returns an error naming the keys md did not decode. An
// unknown table is reported once, not each key inside it.
func unknownKeys(md toml.MetaData) error {
	und := md.Undecoded()
	if len(und) == 0 {
		return nil
	}
	var keys []string
	for _, k := range und {
		s := k.String()
		if !slices.ContainsFunc(keys, func(p string) bool { return strings.HasPrefix(s, p+".") }) {
			keys = append(keys, s)
		}
	}
	if len(keys) == 1 {
		return fmt.Errorf("unknown key %s", keys[0])
	}
	return fmt.Errorf("unknown keys %s", strings.Join(keys, ", "))
}
