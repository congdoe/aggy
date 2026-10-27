// Package adapter defines the interface every vendor CLI implements.
// aggy never calls a vendor CLI except through an Adapter. Adapters build
// commands and parse output; they never start processes. The contract is
// documented in docs/adapters.md.
package adapter
