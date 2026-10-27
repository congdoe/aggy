// Package config loads user config (~/.aggy/config.toml, or under
// $AGGY_HOME) over generic defaults: default vendor, per-vendor model,
// switch order, routing per borrow kind, and budgets. It also loads repo
// config (<repo>/.aggy/config.toml): allowed vendors, check commands, and
// worktree settings. Merge combines the two, and a repo can only narrow
// what the user config allows.
package config
