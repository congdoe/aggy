package router

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	codexadapter "github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/config"
)

// Request asks for the vendors that may run one borrow. It mirrors the
// borrow package's route request field for field, but that one types Kind
// as adapter.Kind where this one takes a string (parsed by Choose), so
// the wiring converts it.
type Request struct {
	// Kind is the borrow kind, such as "research".
	Kind string
	// Parent is the vendor the parent session is on. Empty means none,
	// as for `aggy borrow` run from a plain shell.
	Parent adapter.Vendor
	// Repo is the repo root whose content the borrow may see. Empty means
	// no repo, so no repo config narrows the vendors.
	Repo string
	// Override is the --vendor flag: it replaces the routing list for
	// this one borrow. The skip rules still apply to it.
	Override adapter.Vendor
}

// NotAuthorOrder is the order a "not-author" review route walks. The
// parent's vendor, which wrote the change, is then skipped.
var NotAuthorOrder = []adapter.Vendor{adapter.Codex, adapter.Grok, adapter.Claude}

// Router picks vendors for borrows from the routing table.
type Router struct {
	cfg  config.Config
	deps Deps
}

// New returns a router over the user config cfg. cfg is normally from
// config.Load, which has validated it already; Choose validates the route
// it reads again, so a config built in code cannot slip past.
func New(cfg config.Config, deps Deps) *Router {
	return &Router{cfg: cfg, deps: deps}
}

// Route returns the vendor list for kind k from routing: the configured
// order with duplicates removed, or NotAuthorOrder for a "not-author"
// route. An unknown vendor, or "not-author" outside review, is an error
// naming the routing key.
func Route(routing config.Routing, k adapter.Kind) ([]adapter.Vendor, error) {
	r := routing.For(k)
	if r.NotAuthor {
		if k != adapter.Review {
			return nil, fmt.Errorf("routing.%s: %q is only valid for review", k, config.NotAuthor)
		}
		return slices.Clone(NotAuthorOrder), nil
	}
	out := make([]adapter.Vendor, 0, len(r.Vendors))
	for _, v := range r.Vendors {
		if _, err := adapter.ParseVendor(string(v)); err != nil {
			return nil, fmt.Errorf("routing.%s: %w", k, err)
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, nil
}

// Refusal returns why vendor v's adapter refuses kind k even when the
// routing table lists it, or nil. The router keeps such a vendor in its
// list, since the table is the user's choice, and reports this reason so
// the adapter's refusal is not a surprise.
func Refusal(k adapter.Kind, v adapter.Vendor) error {
	// TODO(T6638): codexadapter.Refusal holds the one message and the reason.
	if v == adapter.Codex {
		return codexadapter.Refusal(k)
	}
	return nil
}

// ErrNoVendor matches the error Choose returns when every vendor in the
// route was skipped or the route is empty.
var ErrNoVendor = errors.New("no vendor available")

// Skip is one vendor Choose left out, and why.
type Skip struct {
	Vendor adapter.Vendor
	Reason string
}

// NoVendorError says why no vendor is left for a kind. It matches
// ErrNoVendor.
type NoVendorError struct {
	Kind    adapter.Kind
	Skipped []Skip // in route order; empty when the route itself is empty
}

func (e *NoVendorError) Error() string {
	if len(e.Skipped) == 0 {
		return fmt.Sprintf("%s for %s: [routing] %s lists no vendor", ErrNoVendor, e.Kind, e.Kind)
	}
	parts := make([]string, len(e.Skipped))
	for i, s := range e.Skipped {
		parts[i] = fmt.Sprintf("%s: %s", s.Vendor, s.Reason)
	}
	return fmt.Sprintf("%s for %s: %s", ErrNoVendor, e.Kind, strings.Join(parts, "; "))
}

// Is makes errors.Is(err, ErrNoVendor) true.
func (e *NoVendorError) Is(target error) bool { return target == ErrNoVendor }

// Decision is the full result of a route: the candidates in order, the
// vendors skipped and why, and notes about candidates the caller should
// show, such as an unknown sign-in status or an adapter refusal.
type Decision struct {
	Kind       adapter.Kind
	Candidates []adapter.Vendor
	Skipped    []Skip
	Notes      []string
	// Warnings must be shown to the person: a vendor kept against an
	// unconfirmed limit (see skips).
	Warnings []string
}

// Choose returns the vendors that may run req, best first. When none is
// left it returns an error matching ErrNoVendor that says why each vendor
// was skipped.
func (r *Router) Choose(ctx context.Context, req Request) ([]adapter.Vendor, error) {
	d, err := r.Decide(ctx, req)
	if err != nil {
		return nil, err
	}
	return d.Candidates, nil
}

// Decide is Choose with the reasons kept. Its error is the same as
// Choose's.
func (r *Router) Decide(ctx context.Context, req Request) (Decision, error) {
	k, err := adapter.ParseKind(req.Kind)
	if err != nil {
		return Decision{}, err
	}
	if req.Parent != "" {
		if _, err := adapter.ParseVendor(string(req.Parent)); err != nil {
			return Decision{}, fmt.Errorf("parent: %w", err)
		}
	}
	var list []adapter.Vendor
	if req.Override != "" {
		if _, err := adapter.ParseVendor(string(req.Override)); err != nil {
			return Decision{}, fmt.Errorf("--vendor: %w", err)
		}
		list = []adapter.Vendor{req.Override}
	} else if list, err = Route(r.cfg.Routing, k); err != nil {
		return Decision{}, err
	}
	d := Decision{Kind: k}
	if len(list) == 0 {
		return Decision{}, &NoVendorError{Kind: k}
	}
	skips, notes, warnings, err := r.skips(ctx, req, k, list)
	if err != nil {
		return Decision{}, err
	}
	d.Warnings = warnings
	for _, v := range list {
		if reason, ok := skips[v]; ok {
			d.Skipped = append(d.Skipped, Skip{Vendor: v, Reason: reason})
			continue
		}
		d.Candidates = append(d.Candidates, v)
		if n := notes[v]; n != "" {
			d.Notes = append(d.Notes, fmt.Sprintf("%s: %s", v, n))
		}
		if e := Refusal(k, v); e != nil {
			d.Notes = append(d.Notes, e.Error())
		}
	}
	if len(d.Candidates) == 0 {
		return Decision{}, &NoVendorError{Kind: k, Skipped: d.Skipped}
	}
	return d, nil
}
