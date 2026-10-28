//go:build race

package redact

// raceOn is true under -race, where regexps run far slower.
const raceOn = true
