package importer

import (
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
)

// openPTY opens a pseudo-terminal pair (linux /dev/ptmx).
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	var unlock int32
	if err := ioctl(m.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		m.Close()
		t.Skipf("unlockpt: %v", err)
	}
	var n uint32
	if err := ioctl(m.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		m.Close()
		t.Skipf("ptsname: %v", err)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Skipf("open pts: %v", err)
	}
	t.Cleanup(func() { s.Close(); m.Close() })
	return m, s
}

// TestExactFPTerminalEchoOff: on a terminal, echo is off while the value is read, nothing of it is
// echoed back, the terminal is restored, and stdout is only the fingerprint.
func TestExactFPTerminalEchoOff(t *testing.T) {
	master, slave := openPTY(t)
	before, err := getTermios(slave.Fd())
	if err != nil || before.Lflag&syscall.ECHO == 0 {
		t.Skipf("pty without echo: %v", err)
	}
	x := newH(t)
	x.env.Stdin = slave
	echoOff := make(chan bool, 1)
	echoOffHook = func() {
		tm, err := getTermios(slave.Fd())
		echoOff <- err == nil && tm.Lflag&syscall.ECHO == 0
	}
	defer func() { echoOffHook = nil }()
	done := make(chan int, 1)
	go func() { done <- Main([]string{"exact-fp"}, x.env) }()
	select {
	case off := <-echoOff:
		if !off {
			t.Fatal("echo was on while reading the value")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("exact-fp never read the terminal")
	}
	value := "terminal-typed-s3cr3t-value"
	if _, err := master.Write([]byte(value + "\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case rc := <-done:
		if rc != 0 {
			t.Fatalf("rc %d: %s", rc, x.errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("exact-fp did not finish")
	}
	fp := fingerprint.ExactFP(value)
	if x.out.String() != hex.EncodeToString(fp[:])+"\n" {
		t.Fatalf("stdout %q", x.out.String())
	}
	if strings.Contains(x.errb.String(), value) {
		t.Fatal("the value reached stderr")
	}
	// Nothing was echoed to the terminal side (a non-blocking read of what the pty sent back).
	time.Sleep(100 * time.Millisecond)
	mfd := int(master.Fd())
	if err := syscall.SetNonblock(mfd, true); err != nil {
		t.Fatal(err)
	}
	var echoed []byte
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(mfd, buf)
		if n <= 0 || err != nil {
			break
		}
		echoed = append(echoed, buf[:n]...)
	}
	if strings.Contains(string(echoed), value) {
		t.Fatal("the value was echoed")
	}
	after, err := getTermios(slave.Fd())
	if err != nil || after.Lflag != before.Lflag {
		t.Fatal("terminal flags not restored")
	}
}

// TestMaskReviewRefusesOnTerminalWithoutFlag: a terminal on stdout is not enough; the flag is required.
func TestMaskReviewRefusesOnTerminalWithoutFlag(t *testing.T) {
	_, slave := openPTY(t)
	x := newH(t)
	x.writeCorpus()
	x.env.Stdout = slave
	x.env.Getenv = func(string) string { return "" }
	if rc := Main([]string{"mask-review", "--rule", "generic-api-key", "--sample", "5"}, x.env); rc == 0 {
		t.Fatal("mask-review ran on a terminal without CHAT_ARCHIVE_MASK_REVIEW_OPERATOR=1")
	}
	if !strings.Contains(x.errb.String(), "the operator's own terminal") {
		t.Fatalf("stderr %q", x.errb.String())
	}
}
