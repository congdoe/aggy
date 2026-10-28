package router

import (
	"context"
	"fmt"

	"github.com/congdoe/aggy/internal/adapter"
)

// Deps are the router's inputs besides config.
type Deps struct{}

// skips returns, per vendor in list, why it is left out, and notes on the
// vendors kept. A review leaves out its author: the parent's vendor.
func (r *Router) skips(_ context.Context, req Request, k adapter.Kind, list []adapter.Vendor) (map[adapter.Vendor]string, map[adapter.Vendor]string, error) {
	skip := map[adapter.Vendor]string{}
	for _, v := range list {
		if k == adapter.Review && v == req.Parent {
			skip[v] = fmt.Sprintf("the parent is on %s, which wrote the change", v)
		}
	}
	return skip, nil, nil
}
