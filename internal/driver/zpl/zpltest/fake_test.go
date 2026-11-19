package zpltest

import (
	"net"
	"testing"
	"time"
)

// TCP is a byte stream: a command split across reads still gets its answer.
func TestSplitCommand(t *testing.T) {
	f := &Fake{}
	c, err := net.Dial("tcp", f.Start(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("~H"))
	time.Sleep(50 * time.Millisecond)
	_, _ = c.Write([]byte("S\r\n"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 256)
	if n, err := c.Read(b); err != nil || n < 20 {
		t.Errorf("no ~HS reply: %q %v", b[:n], err)
	}
}
