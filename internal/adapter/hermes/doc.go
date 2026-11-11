// Package hermes builds Hermes Agent's commands and reads its output. It
// holds every Hermes flag, subcommand, config key, hook payload field, output
// format, and session id rule aggy depends on, each cited from the Hermes
// source at v0.23.1 or a live check in docs/verification.md, and renders the
// block aggy writes into its own Hermes profile's config.yaml. It starts no
// process and writes no file.
//
// Hermes runs on screen only (docs/design.md, section 15, "Hermes"): Headless refuses.
package hermes
