//go:build !unix

package execrun

import (
	"context"
	"errors"
)

// StartProxy needs process groups; on Windows (after 1.0, design section
// 2) a session starts without its proxy.
func StartProxy(context.Context, ProxySpec, []string) (*Proxy, error) {
	return nil, errors.New("execrun: a tool proxy runs on macOS and Linux only")
}
