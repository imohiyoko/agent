package main

import (
	"os"
	"syscall"
	"unsafe"
)

// noEcho turns terminal echo off and returns a function that turns it back
// on. It fails when f is not a terminal.
func noEcho(f *os.File) (func(), error) {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGETA, uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil, e
	}
	old := t
	t.Lflag &^= syscall.ECHO
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSETA, uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil, e
	}
	return func() {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSETA, uintptr(unsafe.Pointer(&old)))
	}, nil
}
