package diag

import (
	"crypto/rand"
	"strings"
	"sync"
	"time"
)

// crockford is the ULID alphabet (Crockford's base32: no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulids makes ULIDs: 48 bits of Unix milliseconds, then 80 random bits,
// as 26 characters that sort by time. Within one millisecond the random
// part counts up, so one process's ids stay in order.
type ulids struct {
	mu   sync.Mutex
	ms   int64
	rand [10]byte
}

// next returns a new ULID for t.
func (g *ulids) next(t time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ms := t.UnixMilli()
	if ms <= g.ms {
		ms = g.ms
		for i := len(g.rand) - 1; i >= 0; i-- {
			g.rand[i]++
			if g.rand[i] != 0 {
				break
			}
		}
	} else {
		g.ms = ms
		_, _ = rand.Read(g.rand[:])
	}
	// 128 bits as hi:lo, read out 5 bits at a time from the low end.
	hi := uint64(ms)<<16 | uint64(g.rand[0])<<8 | uint64(g.rand[1])
	var lo uint64
	for _, b := range g.rand[2:] {
		lo = lo<<8 | uint64(b)
	}
	var out [26]byte
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// ValidID reports whether s is a ULID as aggy writes them: 26 characters
// of the alphabet, the first at most 7 (128 bits).
func ValidID(s string) bool {
	if len(s) != 26 || s[0] > '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(crockford, s[i]) < 0 {
			return false
		}
	}
	return true
}
