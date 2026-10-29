//go:build unix

package execrun

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// This file is the only network code outside the supervisor socket
// (T6780, .golangci.yml): a TCP connect to a tool proxy's loopback
// address, and an HTTP GET of its health path there. Both dial exactly
// the address tooldef.ParseListen accepted (127.0.0.1 or ::1), resolve
// no name, use no HTTP proxy from the environment, and follow no
// redirect.

// dialLoopback dials addr, which must be loopback, whatever address the
// caller asked for.
func dialLoopback(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	if !addr.Addr().IsLoopback() {
		return nil, ErrProxyUnhealthy
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr.String())
}

// portTaken reports whether something accepts connections on addr.
func portTaken(addr netip.AddrPort) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c, err := dialLoopback(ctx, addr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// holdLoopback takes addr after a proxy died mid-session (HIGH-2), so
// no other process can bind it while the CLI still points there: it
// accepts each connection and closes it at once, reading nothing, so no
// request byte (a token among them) is ever received. The returned func
// closes it.
func holdLoopback(addr netip.AddrPort) (func(), error) {
	if !addr.Addr().IsLoopback() {
		return nil, ErrProxyNotOwned
	}
	ln, err := net.Listen("tcp", addr.String())
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return func() { _ = ln.Close() }, nil
}

// healthy reports whether a GET of path at addr answers 2xx within a
// second. A redirect is not followed and counts as unhealthy.
func healthy(ctx context.Context, addr netip.AddrPort, path string) bool {
	c := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			Proxy: nil, // never HTTP_PROXY or the like
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialLoopback(ctx, addr)
			},
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 16 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyURL(addr)+path, nil)
	if err != nil {
		return false
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
