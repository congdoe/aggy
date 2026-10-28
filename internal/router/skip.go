package router

import (
	"context"
	"fmt"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/store"
)

// Statuses reads what aggy setup last found per vendor. *store.Store
// implements it.
type Statuses interface {
	VendorStatuses(ctx context.Context) ([]store.VendorStatus, error)
}

// LimitState says whether a vendor hit a plan limit recently. Until is
// when aggy expects it back, or zero when unknown.
//
// TODO(T6666): the store records no limit signal yet (limit detection
// comes with M2), so the default, NoLimits, reports none.
type LimitState interface {
	Limited(ctx context.Context, v adapter.Vendor, now time.Time) (limited bool, until time.Time, err error)
}

// NoLimits is a LimitState that never reports a limit.
type NoLimits struct{}

// Limited always reports no limit.
func (NoLimits) Limited(context.Context, adapter.Vendor, time.Time) (bool, time.Time, error) {
	return false, time.Time{}, nil
}

// Deps are the router's inputs besides config.
type Deps struct {
	// Statuses gives each vendor's sign-in status, usually the store.
	// Nil treats every vendor as unknown: usable, with a note.
	Statuses Statuses
	// RepoConfig loads the repo config of the repo at root. Nil means
	// config.LoadRepo, which reads through safefs.
	RepoConfig func(root string) (config.RepoConfig, error)
	// Limits reports recent plan limits. Nil means NoLimits.
	Limits LimitState
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Sign-in states from the vendors table (store migration 0006).
const (
	statusConnected    = "connected"
	statusNotConnected = "not_connected"
	statusUnknown      = "unknown"
)

// skips applies the skip rules to list, in this order: the parent's own
// vendor, a vendor not connected, a vendor outside the repo's
// allowed_vendors, and a vendor at its limit. It returns, per vendor, why
// it is left out (the first rule that matched), and notes on vendors
// kept. A rule's input that cannot be read is an error: the router never
// guesses that a repo allows a vendor.
func (r *Router) skips(ctx context.Context, req Request, _ adapter.Kind, list []adapter.Vendor) (map[adapter.Vendor]string, map[adapter.Vendor]string, error) {
	status, err := r.statuses(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read vendor status: %w", err)
	}
	allowed, err := r.allowed(req.Repo)
	if err != nil {
		return nil, nil, err
	}
	limits := r.deps.Limits
	if limits == nil {
		limits = NoLimits{}
	}
	now := time.Now
	if r.deps.Now != nil {
		now = r.deps.Now
	}
	skip := map[adapter.Vendor]string{}
	notes := map[adapter.Vendor]string{}
	for _, v := range list {
		if req.Parent != "" && v == req.Parent {
			skip[v] = fmt.Sprintf("the parent is on %s", v)
			continue
		}
		switch st, ok := status[v]; {
		case !ok:
			skip[v] = "not set up (run aggy setup)"
			continue
		case st == statusConnected:
		case st == statusUnknown:
			notes[v] = "sign-in status unknown; trying it anyway"
		case st == statusNotConnected:
			skip[v] = "not connected"
			continue
		default:
			skip[v] = fmt.Sprintf("sign-in status %q not understood", st)
			continue
		}
		if !allowed.Allows(v) {
			skip[v] = "not in this repo's allowed_vendors"
			continue
		}
		t := now()
		limited, until, err := limits.Limited(ctx, v, t)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: read limit state: %w", v, err)
		}
		if limited {
			if until.After(t) {
				skip[v] = fmt.Sprintf("hit its limit; expected back at %s", until.Local().Format("Jan 2 15:04"))
			} else {
				skip[v] = "hit its limit recently"
			}
		}
	}
	return skip, notes, nil
}

// statuses returns each vendor's recorded sign-in status. Without a
// store every vendor is unknown.
func (r *Router) statuses(ctx context.Context) (map[adapter.Vendor]string, error) {
	out := map[adapter.Vendor]string{}
	if r.deps.Statuses == nil {
		for _, v := range adapter.Vendors() {
			out[v] = statusUnknown
		}
		return out, nil
	}
	rows, err := r.deps.Statuses.VendorStatuses(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		v, err := adapter.ParseVendor(row.Vendor)
		if err != nil {
			continue // the table's CHECK allows only known vendors
		}
		out[v] = row.Connected
	}
	return out, nil
}

// allowed returns the effective config for repo, whose Allows says which
// vendors may see it. No repo allows every vendor.
func (r *Router) allowed(repo string) (config.Effective, error) {
	if repo == "" {
		return config.Merge(r.cfg, config.RepoConfig{}), nil
	}
	load := r.deps.RepoConfig
	if load == nil {
		load = config.LoadRepo
	}
	rc, err := load(repo)
	if err != nil {
		return config.Effective{}, fmt.Errorf("read repo config: %w", err)
	}
	return config.Merge(r.cfg, rc), nil
}
