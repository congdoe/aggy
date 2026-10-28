// Package router picks the vendors for a borrow kind from the routing
// table ([routing] in user config over generic defaults), skipping vendors
// that are disallowed, disconnected, or at their limit. It is aggy's only
// routing logic: no model takes part.
package router
