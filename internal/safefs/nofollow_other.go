//go:build !unix

package safefs

// oNoFollow is zero where O_NOFOLLOW does not exist; the identity check on
// the opened descriptor still applies.
const oNoFollow = 0
