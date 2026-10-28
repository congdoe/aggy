package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/store"
)

// TestAllowedVendorsLaunch: a launch never puts a repo on a vendor its
// allowed_vendors leaves out, the default vendor comes from the allowed
// ones, the switch picker offers only allowed vendors, and a repo config
// that cannot be read refuses (fail closed).
func TestAllowedVendorsLaunch(t *testing.T) {
	s := sessionStore(t)
	cfg := config.Default()
	cfg.DefaultVendor = adapter.Claude
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	setRepo := func(text string) {
		t.Helper()
		mustWrite(t, filepath.Join(repo, ".aggy", "config.toml"), text, 0o600)
	}

	setRepo("allowed_vendors = [\"grok\", \"codex\"]\n")
	if _, err := allowedIn(cfg, sub, adapter.Claude); err == nil || !strings.Contains(err.Error(), "does not list claude") {
		t.Errorf("claude: err = %v, want a refusal", err)
	}
	eff, err := allowedIn(cfg, sub, adapter.Grok)
	if err != nil {
		t.Fatal(err)
	}
	// The repo narrows the user's switch order, keeping its order.
	if !slices.Equal(eff.Switch.Order, []adapter.Vendor{adapter.Codex, adapter.Grok}) {
		t.Errorf("switch order = %v", eff.Switch.Order)
	}
	// It never widens it: a vendor the user's order leaves out stays out.
	user := cfg
	user.Switch.Order = []adapter.Vendor{adapter.Claude, adapter.Grok}
	if eff, _ := allowedIn(user, sub, adapter.Grok); !slices.Equal(eff.Switch.Order, []adapter.Vendor{adapter.Grok}) {
		t.Errorf("narrowed order = %v", eff.Switch.Order)
	}
	// A new session with no vendor named starts on an allowed one, not on
	// the user's default_vendor.
	p, err := plan(context.Background(), s, cfg, sub, target{New: true}, time.Now(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if v := adapter.Vendor(p.child.Vendor); v != adapter.Codex && v != adapter.Grok {
		t.Errorf("default vendor = %s, want an allowed one", v)
	}

	// aggy resume <child-id>, from anywhere: the child's own repo decides.
	seedParent(t, s, "dddd0000-0000-4000-8000-000000000000", sub,
		store.Child{ID: "dd11child", Vendor: "claude", VendorSessionID: sidA})
	p, err = plan(context.Background(), s, cfg, t.TempDir(), target{ParentID: "dd11"}, time.Now(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allowedIn(cfg, p.child.Cwd, adapter.Vendor(p.child.Vendor)); p.child.Vendor != "claude" || err == nil {
		t.Errorf("resume by child id: child %+v, err %v; want claude refused", p.child, err)
	}

	setRepo("allowed_vendors = []\n")
	if _, err := allowedIn(cfg, sub, adapter.Grok); err == nil {
		t.Error("empty allowed_vendors: want a refusal")
	}
	setRepo("allowed_vendors = \"grok\"\n")
	if _, err := allowedIn(cfg, sub, adapter.Grok); err == nil || !strings.Contains(err.Error(), "repo config") {
		t.Errorf("unreadable config: err = %v, want a refusal", err)
	}
	if _, err := plan(context.Background(), s, cfg, sub, target{New: true}, time.Now(), &bytes.Buffer{}); err == nil {
		t.Error("plan with an unreadable repo config: want an error")
	}
	// Outside a repo with no config, every vendor is allowed.
	if _, err := allowedIn(cfg, t.TempDir(), adapter.Claude); err != nil {
		t.Errorf("no repo: %v", err)
	}
}
