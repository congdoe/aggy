package execrun

import (
	"encoding/binary"
	"net/netip"
	"slices"
	"testing"
)

// TestProcNet: the /proc/net/tcp parsing behind listenerOwned.
func TestProcNet(t *testing.T) {
	le := binary.NativeEndian.Uint16([]byte{1, 0}) == 1
	v4, v6 := "0100007F:2253", "00000000000000000000000001000000:2253"
	if !le {
		v4, v6 = "7F000001:2253", "00000000000000000000000000000001:2253"
	}
	if got := procNetAddr(netip.MustParseAddrPort("127.0.0.1:8787")); got != v4 {
		t.Errorf("v4 = %s, want %s", got, v4)
	}
	if got := procNetAddr(netip.MustParseAddrPort("[::1]:8787")); got != v6 {
		t.Errorf("v6 = %s, want %s", got, v6)
	}
	table := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: " + v4 + " 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242 1 0 100 0 0 10 0\n" +
		"   1: " + v4 + " 0100007F:C350 01 00000000:00000000 00:00000000 00000000  1000        0 4343 1 0 20 4 30 10 -1\n" +
		"   2: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4444 1 0 100 0 0 10 0\n"
	if got := listenInodes([]byte(table), v4); !slices.Equal(got, []string{"4242"}) {
		t.Errorf("inodes = %v", got)
	}
	for _, c := range []struct {
		stat string
		want int
	}{
		{"123 (headroom) S 1 123 123 0 -1", 123},
		{"124 (a) b) c) R 123 777 123 0", 777},
		{"garbage", -1},
	} {
		if got := statPgrp([]byte(c.stat)); got != c.want {
			t.Errorf("statPgrp(%q) = %d, want %d", c.stat, got, c.want)
		}
	}
}
