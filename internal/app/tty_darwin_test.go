//go:build darwin

package app

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY opens a pseudo-terminal pair the way posix_openpt, grantpt,
// unlockpt and ptsname do on macOS.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	ioctl := func(req uintptr, arg unsafe.Pointer) syscall.Errno {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), req, uintptr(arg))
		return errno
	}
	var name [128]byte
	if e := ioctl(syscall.TIOCPTYGRANT, nil); e != 0 {
		m.Close()
		t.Skipf("grantpt: %v", e)
	}
	if e := ioctl(syscall.TIOCPTYUNLK, nil); e != 0 {
		m.Close()
		t.Skipf("unlockpt: %v", e)
	}
	if e := ioctl(syscall.TIOCPTYGNAME, unsafe.Pointer(&name[0])); e != 0 {
		m.Close()
		t.Skipf("ptsname: %v", e)
	}
	n := bytes.IndexByte(name[:], 0)
	if n < 0 {
		n = len(name)
	}
	s, err := os.OpenFile(string(name[:n]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Skipf("open %s: %v", name[:n], err)
	}
	t.Cleanup(func() { s.Close(); m.Close() })
	return m, s
}

func TestInteractiveAcceptsAPseudoTerminal(t *testing.T) {
	_, slave := openPTY(t)
	if !Interactive(slave) {
		t.Fatal("a pseudo-terminal is a terminal")
	}
}
