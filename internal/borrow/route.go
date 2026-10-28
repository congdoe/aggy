package borrow

import (
	"context"
	"errors"

	"github.com/congdoe/aggy/internal/adapter"
)

// Router picks the vendors for a borrow, best first, after its skip
// rules (the parent's vendor, not connected, outside the repo's
// allowed_vendors, at its limit). internal/cli wires internal/router in;
// this package depends only on the interface.
type Router interface {
	Choose(ctx context.Context, req RouteRequest) ([]adapter.Vendor, error)
}

// RouteRequest is what a Router decides on.
type RouteRequest struct {
	Kind adapter.Kind
	// Parent is the vendor on screen, which should use its own tools;
	// "" when unknown.
	Parent adapter.Vendor
	// Repo is the repo root the borrow may see: the git work tree root,
	// or the directory aggy borrow ran in when it is in none.
	Repo string
	// Override is the --vendor the caller named, "" for none. It replaces
	// the routing list, and the skip rules still apply to it.
	Override adapter.Vendor
}

// RouterFunc adapts a function to Router.
type RouterFunc func(ctx context.Context, req RouteRequest) ([]adapter.Vendor, error)

// Choose calls f.
func (f RouterFunc) Choose(ctx context.Context, req RouteRequest) ([]adapter.Vendor, error) {
	return f(ctx, req)
}

// Supports returns nil when a can build a borrow of kind k, and the
// adapter's reason otherwise (matching adapter.ErrUnsupported when the CLI
// cannot serve the kind). It only builds a command, from placeholder
// paths: adapters touch no disk while building.
func Supports(a adapter.Adapter, k adapter.Kind) error {
	req := adapter.HeadlessRequest{
		Kind: k, PromptFile: "/aggy-support-check/run/prompt.md", Cwd: "/aggy-support-check/work",
		RunDir:      "/aggy-support-check/run",
		IsolatedCwd: true, WebSearch: k == adapter.Research || k == adapter.News,
	}
	_, err := a.Headless(req)
	if errors.Is(err, adapter.ErrFileTools) {
		// Supported once the user opts in; runOne refuses with the hint.
		return nil
	}
	return err
}
