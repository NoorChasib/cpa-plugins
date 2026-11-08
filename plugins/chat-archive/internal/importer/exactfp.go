package importer

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
)

// exactFP reads one value from stdin and prints hex(ExactFP(value)) and nothing else, for the archive's
// extra exact fingerprints. On a terminal, echo is off while the value is typed and a prompt goes to
// stderr; piped stdin works too. The value is never printed, logged or sent.
func (e *Env) exactFP() error {
	var line string
	var err error
	if f, ok := e.Stdin.(*os.File); ok && isTerminal(f.Fd()) {
		fmt.Fprint(e.Stderr, "value (not echoed): ")
		line, err = readNoEcho(f)
		fmt.Fprintln(e.Stderr)
	} else {
		line, err = bufio.NewReader(e.Stdin).ReadString('\n')
		if errors.Is(err, io.EOF) && line != "" {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	v := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if v == "" {
		return errors.New("no value on stdin")
	}
	if reason := exactUnmatchable(v); reason != "" {
		// Its fingerprint could never mask anything; say why without the value.
		return fmt.Errorf("%s, so exact-value masking can never match it; no fingerprint printed", reason)
	}
	fp := fingerprint.ExactFP(v)
	fmt.Fprintln(e.Stdout, hex.EncodeToString(fp[:]))
	return nil
}

// exactUnmatchable says why mask/v1 could never find v as an exact value, or "" when it can: exact
// candidates are runs of [A-Za-z0-9_-+/=.~] of 20 to 512 bytes (pkg/mask exact.go).
// The reason names a limit, never the value or its length.
func exactUnmatchable(v string) string {
	switch {
	case len(v) < 20:
		return "the value is shorter than 20 bytes"
	case len(v) > 512:
		return "the value is longer than 512 bytes"
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("_-+/=.~", c) >= 0) {
			return "the value contains characters outside [A-Za-z0-9_-+/=.~]"
		}
	}
	return ""
}

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

func getTermios(fd uintptr) (*syscall.Termios, error) {
	var t syscall.Termios
	if err := ioctl(fd, ioctlGetTermios, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return &t, nil
}

func isTerminal(fd uintptr) bool {
	_, err := getTermios(fd)
	return err == nil
}

// readNoEcho reads one line from a terminal with ECHO off, restoring the terminal afterwards.
func readNoEcho(f *os.File) (string, error) {
	fd := f.Fd()
	old, err := getTermios(fd)
	if err != nil {
		return "", err
	}
	quiet := *old
	quiet.Lflag &^= syscall.ECHO
	quiet.Lflag |= syscall.ICANON | syscall.ISIG
	if err := ioctl(fd, ioctlSetTermios, unsafe.Pointer(&quiet)); err != nil {
		return "", err
	}
	defer ioctl(fd, ioctlSetTermios, unsafe.Pointer(old))
	if echoOffHook != nil {
		echoOffHook()
	}
	var b []byte
	buf := make([]byte, 1)
	for {
		n, err := f.Read(buf)
		if n == 1 {
			if buf[0] == '\n' || buf[0] == '\r' {
				break
			}
			b = append(b, buf[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(b) > 0 {
				break
			}
			return "", err
		}
	}
	return string(b), nil
}

// echoOffHook runs once echo is off (tests check the terminal state there).
var echoOffHook func()

// isTTY reports whether w is a terminal.
func isTTY(w any) bool {
	f, ok := w.(*os.File)
	return ok && isTerminal(f.Fd())
}
