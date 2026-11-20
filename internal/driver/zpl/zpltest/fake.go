// Package zpltest is a fake Zebra printer on 127.0.0.1 for tests: it answers the queries the zpl
// driver and `pdb-print try` send, and records everything it receives.
package zpltest

import (
	"bytes"
	_ "embed"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// Real replies of a GK420d (V61.17.11Z, 102×102 mm labels, print width 799) from a probe run on
// 2026-11-18, with its serial number, name, addresses and MACs replaced by made-up ones.
var (
	//go:embed testdata/hi
	realHI string
	//go:embed testdata/hqsn
	realHQSN string
	//go:embed testdata/hh
	realHH string
	//go:embed testdata/hza
	realHZA string
)

// Fake answers like that GK420d: ~HI, ~HQSN, ^HH and ^HZA with its real replies, ~HS and ~HQES
// in its format but from the fields below. Change fields with Set while it runs.
type Fake struct {
	Model         string        // ~HI model part; "" = GK420d-200dpi
	DPM           int           // dots per mm in ~HI; 0 = 8
	LabelDots     int           // ~HS label length; 0 = 812
	PrintWidth    int           // ^HH PRINT WIDTH; 0 = 799
	Errors        uint64        // ~HQES error bits (group 2 << 32 | group 1)
	PaperOut      bool          // ~HS flags
	Paused        bool          //
	PartialFormat bool          // "partial format in progress"
	HeadUp        bool          //
	SilentHS      bool          // no ~HS reply (as a Zebra in an error state)
	SilentHI      bool          // no ~HI reply (not a Zebra, or a Zebra in EPL mode)
	NoHQES        bool          // old firmware without ~HQES
	Hold          time.Duration // how long a received label stays in the buffer; 0 = 300 ms
	Invisible     bool          // labels never show in the buffer (printed before ~HS could see them)

	mu       sync.Mutex
	received bytes.Buffer
	formats  []string
	pending  int
}

// Start listens on 127.0.0.1 until the test ends and returns host:port.
func (f *Fake) Start(t testing.TB) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return ln.Addr().String()
}

// Set changes the fake's state while it runs.
func (f *Fake) Set(change func(f *Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// Received is everything sent to the fake so far.
func (f *Fake) Received() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.received.String()
}

// Formats are the ^XA…^XZ formats received that print something (^GFA), in order.
func (f *Fake) Formats() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.formats...)
}

// AllFormats are every ^XA…^XZ received, including setup-only ones.
func (f *Fake) AllFormats() []string {
	var out []string
	s := f.Received()
	for {
		i := strings.Index(s, "^XA")
		if i < 0 {
			return out
		}
		j := strings.Index(s[i:], "^XZ")
		if j < 0 {
			return out
		}
		out = append(out, s[i:i+j+3])
		s = s[i+j+3:]
	}
}

func (f *Fake) serve(c net.Conn) {
	defer c.Close()
	var buf string
	b := make([]byte, 64<<10)
	for {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := c.Read(b)
		if n > 0 {
			f.mu.Lock()
			f.received.Write(b[:n])
			f.mu.Unlock()
			buf += string(b[:n])
			buf = f.handle(c, buf)
		}
		if err != nil {
			return
		}
	}
}

// handle answers every complete command in buf and returns what is left.
func (f *Fake) handle(c net.Conn, buf string) string {
	for buf != "" {
		switch {
		case strings.HasPrefix(buf, "~HQES"):
			buf = buf[5:]
			f.reply(c, f.hqes())
		case strings.HasPrefix(buf, "~HQSN"):
			buf = buf[5:]
			f.reply(c, realHQSN)
		case strings.HasPrefix(buf, "~HI"):
			buf = buf[3:]
			f.reply(c, f.hi())
		case strings.HasPrefix(buf, "~HS"):
			buf = buf[3:]
			f.reply(c, f.hs())
		case strings.HasPrefix(buf, "^XA"):
			end := strings.Index(buf, "^XZ")
			if end < 0 {
				return buf // the rest of the format is still coming
			}
			f.format(c, buf[:end+3])
			buf = buf[end+3:]
		default:
			for _, cmd := range []string{"~HQES", "~HQSN", "~HI", "~HS", "^XA"} {
				if len(buf) < len(cmd) && strings.HasPrefix(cmd, buf) {
					return buf // the rest of the command is still coming
				}
			}
			buf = buf[1:] // CR, LF and anything we don't know
		}
	}
	return buf
}

func (f *Fake) format(c net.Conn, fm string) {
	switch {
	case strings.Contains(fm, "^HH"):
		hh := f.withWidth(realHH, "%d                 PRINT WIDTH")
		f.reply(c, "\x02"+strings.Replace(hh, "0812                LABEL LENGTH", fmt.Sprintf("%04d                LABEL LENGTH", f.labelDots()), 1)+"\x03\r\n")
	case strings.Contains(fm, "^HZA"):
		f.reply(c, f.withWidth(realHZA, "<CURRENT>%d</CURRENT>"))
	case strings.Contains(fm, "^GF"):
		f.mu.Lock()
		f.formats = append(f.formats, fm)
		hold, invisible := f.Hold, f.Invisible
		if !invisible {
			f.pending++
		}
		f.mu.Unlock()
		if hold == 0 {
			hold = 300 * time.Millisecond
		}
		if !invisible {
			time.AfterFunc(hold, func() { f.Set(func(f *Fake) { f.pending-- }) })
		}
	}
}

func (f *Fake) reply(c net.Conn, s string) {
	if s != "" {
		_, _ = c.Write([]byte(s))
	}
}

func (f *Fake) hi() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SilentHI {
		return ""
	}
	if f.Model == "" && f.DPM == 0 {
		return realHI
	}
	model := f.Model
	if model == "" {
		model = "GK420d-200dpi"
	}
	return fmt.Sprintf("\x02%s,V61.17.11Z,%d,2104KB\x03\r\n", model, or(f.DPM, 8))
}

func (f *Fake) labelDots() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return or(f.LabelDots, 812)
}

// withWidth puts the fake's print width into a real reply (the first match of the 799 pattern).
func (f *Fake) withWidth(reply, pattern string) string {
	f.mu.Lock()
	w := or(f.PrintWidth, 799)
	f.mu.Unlock()
	return strings.Replace(reply, fmt.Sprintf(pattern, 799), fmt.Sprintf(pattern, w), 1)
}

func (f *Fake) hs() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SilentHS {
		return ""
	}
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	return fmt.Sprintf("\x02030,%d,%d,%04d,%03d,0,0,%d,000,0,0,0\x03\r\n\x02000,0,%d,0,0,2,6,0,%08d,1,000\x03\r\n\x021234,0\x03\r\n",
		b(f.PaperOut), b(f.Paused), or(f.LabelDots, 812), f.pending, b(f.PartialFormat), b(f.HeadUp), f.pending)
}

func (f *Fake) hqes() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.NoHQES {
		return ""
	}
	flag := 0
	if f.Errors != 0 {
		flag = 1
	}
	return fmt.Sprintf("\x02\r\n\r\n  PRINTER STATUS                            \r\n   ERRORS:         %d %08X %08X      \r\n   WARNINGS:       0 00000000 00000000      \r\n\x03\r\n",
		flag, f.Errors>>32, f.Errors&0xffffffff)
}

func or(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
