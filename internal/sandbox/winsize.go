package sandbox

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

// winsize is struct winsize from <sys/ioctl.h>.
type winsize struct{ Row, Col, X, Y uint16 }

func getWinsize(f *os.File) (winsize, bool) {
	var w winsize
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&w)))
	return w, e == 0
}

func isTerminal(f *os.File) bool {
	_, ok := getWinsize(f)
	return ok
}

// watchSize sends SIGWINCH to p whenever the terminal's size changes.
//
// Inside bwrap nothing tells sandbox-init that the person resized the window:
// SIGWINCH goes to the terminal's foreground process group (nsctl shell),
// and bwrap does not pass signals on. The size itself is on the terminal, so
// it is checked a few times a second instead.
func watchSize(f *os.File, p *os.Process) {
	last, _ := getWinsize(f)
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		w, ok := getWinsize(f)
		if !ok {
			return
		}
		if w != last {
			last = w
			if p.Signal(syscall.SIGWINCH) != nil {
				return
			}
		}
	}
}
