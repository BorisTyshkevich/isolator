//go:build linux

package proxy

import (
	"net"

	"golang.org/x/sys/unix"
)

// GetPeerUID returns the UID of the process on the remote end of conn (a Unix
// socket). On lookup failure it returns ok=false; callers must allow the
// connection in that case (§4 defense-in-depth note).
func GetPeerUID(conn *net.UnixConn) (uint32, bool) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var peerUID uint32
	var sysErr error
	ctlErr := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			sysErr = err
			return
		}
		peerUID = cred.Uid
	})
	if ctlErr != nil || sysErr != nil {
		return 0, false
	}
	return peerUID, true
}
