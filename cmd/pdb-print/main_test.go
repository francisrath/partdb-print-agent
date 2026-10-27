package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func status(statusType byte, err2 byte) []byte {
	b := make([]byte, 32)
	b[0], b[1], b[2], b[3], b[4] = 0x80, 0x20, 'B', '4', 0x39 // QL-810W
	b[9], b[10], b[11], b[18] = err2, 29, 0x0a, statusType     // 29 mm continuous
	return b
}

// fakeQL answers the status request, then reports the page printed. It never touches hardware.
func fakeQL(t *testing.T, coverOpen bool) (addr string, got chan []byte) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var rx bytes.Buffer
		buf := make([]byte, 4096)
		replied := false
		for {
			n, err := c.Read(buf)
			rx.Write(buf[:n])
			if !replied && bytes.Contains(rx.Bytes(), []byte{0x1b, 'i', 'S'}) {
				replied = true
				if coverOpen {
					c.Write(status(0x02, 0x10))
				} else {
					c.Write(status(0x00, 0))
				}
			}
			if bytes.HasSuffix(rx.Bytes(), []byte{0x1a}) {
				c.Write(status(0x01, 0))
			}
			if err != nil {
				got <- rx.Bytes()
				return
			}
		}
	}()
	return ln.Addr().String(), got
}

func writeConfig(t *testing.T, addr string) string {
	host, port, _ := net.SplitHostPort(addr)
	p := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("printers:\n  - id: fake\n    driver: brother-ql\n    model: QL-810W\n    transport: {kind: tcp, host: %s, port: %s}\n    media: dk-22210\n", host, port)
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrintToFakePrinter(t *testing.T) {
	addr, got := fakeQL(t, false)
	cfg := writeConfig(t, addr)
	if err := run(context.Background(), []string{"--config", cfg, "print", "../../testdata/sample-29x46.pdf", "--printer", "fake"}); err != nil {
		t.Fatal(err)
	}
	rx := <-got
	// status request, then the job: 485 raster lines for a 29×46 page on 29 mm tape
	if !bytes.Contains(rx, []byte{0x1b, 'i', 'z', 0xce, 0x0a, 29, 0, 0xe5, 0x01, 0, 0}) {
		t.Errorf("print info for 485 lines missing")
	}
	if c := bytes.Count(rx, []byte{'g', 0}); c < 485 {
		t.Errorf("%d raster lines", c)
	}
}

func TestPrintRefusesWhenCoverOpen(t *testing.T) {
	addr, _ := fakeQL(t, true)
	cfg := writeConfig(t, addr)
	err := run(context.Background(), []string{"--config", cfg, "print", "../../testdata/sample-29x46.pdf", "--printer", "fake"})
	if err == nil || !strings.Contains(err.Error(), "COVER_OPEN") {
		t.Errorf("err = %v", err)
	}
}

func TestDryRunLandscape(t *testing.T) {
	cfg := writeConfig(t, "127.0.0.1:1")
	out := filepath.Join(t.TempDir(), "job.bin")
	if err := run(context.Background(), []string{"--config", cfg, "print", "--dry-run", out, "--printer", "fake", "../../testdata/sample-50x29.pdf"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !bytes.HasSuffix(b, []byte{0x1a}) || bytes.Count(b, []byte{'g', 0}) < 528 {
		t.Errorf("unexpected dry-run output (%d bytes)", len(b))
	}
}
