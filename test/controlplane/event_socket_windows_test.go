package controlplane

import "syscall"

func setSlowReaderReceiveBuffer(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 64<<10)
}
