//go:build linux

package server

import (
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"
)

var (
	pkgUserOnce sync.Once
	pkgUserUID  uint32
	hasPkgUser  bool
)

func lookupPkgUserUID() (uint32, bool) {
	pkgUserOnce.Do(func() {
		u, err := user.Lookup("fn_nspawn")
		if err != nil || u == nil {
			return
		}
		if parsed, err := strconv.ParseUint(u.Uid, 10, 32); err == nil {
			pkgUserUID = uint32(parsed)
			hasPkgUser = true
		}
	})
	return pkgUserUID, hasPkgUser
}

func inspectUnixPeerAllowed(uc *net.UnixConn) bool {
	if os.Getenv("FN_NSPAWN_SOCKET_ALLOW_ALL") == "1" {
		return true
	}
	rawConn, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *syscall.Ucred
	var credErr error
	if cErr := rawConn.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); cErr != nil || credErr != nil || cred == nil {
		return false
	}

	uid := cred.Uid
	if uid == 0 || uid == uint32(os.Geteuid()) {
		return true
	}
	if pkgUID, ok := lookupPkgUserUID(); ok && uid == pkgUID {
		return true
	}
	// On Debian/fnOS, system daemons (e.g. www-data=33, nginx, trim gateway) use UID < 1000,
	// whereas regular NAS users (>= 1000) and nobody (65534) have UID >= 1000.
	return uid < 1000
}
