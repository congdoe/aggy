// Package config loads user config (~/.aggy/config.toml, or under
// $AGGY_HOME) over generic defaults: default vendor, per-vendor model,
// switch order, routing per borrow kind, and budgets. Repo config
// (<repo>/.aggy/config.toml) comes later.
package config
