//go:build !linux

package server

import "net"

func inspectUnixPeerAllowed(_ *net.UnixConn) bool {
	return true
}
