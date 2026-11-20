package zpl

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/francisrath/partdb-print-agent/internal/driver"
)

// parts splits a reply into its <STX>…<ETX> parts (no STX: the whole reply).
func parts(reply []byte) []string {
	var out []string
	for _, p := range bytes.Split(reply, []byte{0x02}) {
		if s := strings.TrimSpace(string(bytes.SplitN(p, []byte{0x03}, 2)[0])); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// hs is the ~HS host status (ZPL II Programming Guide, ~HS).
type hs struct {
	PaperOut, Paused, HeadUp, RibbonOut, BufferFull, PartialFormat bool
	LabelLengthDots, Formats, LabelsRemaining                      int
}

// parseHS reads string 1 (aaa,b,c,dddd,eee,f,g,h,…) and string 2 (mmm,n,o,p,q,r,s,t,uuuuuuuu,…).
func parseHS(reply []byte) (hs, error) {
	p := parts(reply)
	if len(p) < 2 {
		return hs{}, fmt.Errorf("~HS reply has %d parts, want 3", len(p))
	}
	a, b := strings.Split(p[0], ","), strings.Split(p[1], ",")
	if len(a) < 8 || len(b) < 9 {
		return hs{}, fmt.Errorf("~HS reply too short: %q", p[:2])
	}
	num := func(s string) (int, error) { return strconv.Atoi(strings.TrimSpace(s)) }
	length, err1 := num(a[3])
	formats, err2 := num(a[4])
	remaining, err3 := num(b[8])
	if err := errors.Join(err1, err2, err3); err != nil {
		return hs{}, fmt.Errorf("~HS reply: %w", err)
	}
	on := func(s string) bool { return strings.TrimSpace(s) == "1" }
	return hs{PaperOut: on(a[1]), Paused: on(a[2]), BufferFull: on(a[5]), PartialFormat: on(a[7]),
		HeadUp: on(b[2]), RibbonOut: on(b[3]),
		LabelLengthDots: length, Formats: formats, LabelsRemaining: remaining}, nil
}

// hqes is the ~HQES error and warning bits (group 2 << 32 | group 1).
type hqes struct{ Errors, Warnings uint64 }

func parseHQES(reply []byte) (hqes, error) {
	var q hqes
	var seen int
	for _, line := range strings.Split(string(reply), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 || (f[0] != "ERRORS:" && f[0] != "WARNINGS:") {
			continue
		}
		hi, err1 := strconv.ParseUint(f[2], 16, 32)
		lo, err2 := strconv.ParseUint(f[3], 16, 32)
		if err := errors.Join(err1, err2); err != nil {
			return hqes{}, fmt.Errorf("~HQES reply: %w", err)
		}
		v := hi<<32 | lo
		if f[0] == "ERRORS:" {
			q.Errors = v
		} else {
			q.Warnings = v
		}
		seen++
	}
	if seen == 0 {
		return hqes{}, errors.New("~HQES reply has no ERRORS line")
	}
	return q, nil
}

// code maps the first error bit that applies to a driver code (guide, ~HQES error flags).
func (q hqes) code() (driver.Code, string) {
	for _, e := range []struct {
		bit    uint64
		code   driver.Code
		detail string
	}{
		{0x1, driver.NoMedia, "out of labels"},
		{0x2, driver.RibbonOut, "ribbon out"},
		{0x4, driver.CoverOpen, "print head open"},
		{0x8, driver.CutterJam, "cutter fault"},
		{0x10, driver.Overheat, "print head too hot"},
		{0x20, driver.Overheat, "motor too hot"},
		{0x10000, driver.Paused, "printer is paused: press the feed button"},
	} {
		if q.Errors&e.bit != 0 {
			return e.code, e.detail
		}
	}
	if q.Errors != 0 {
		return driver.Unknown, fmt.Sprintf("printer error flags %016X", q.Errors)
	}
	return driver.OK, ""
}

// parseHI reads ~HI: "GK420d-200dpi,V61.17.11Z,8,2104KB" → model part and dots per mm.
func parseHI(reply []byte) (model string, dpm int, err error) {
	p := parts(reply)
	if len(p) == 0 {
		return "", 0, errors.New("empty ~HI reply")
	}
	f := strings.Split(p[0], ",")
	if len(f) < 3 {
		return "", 0, fmt.Errorf("~HI reply %q", p[0])
	}
	dpm, err = strconv.Atoi(strings.TrimSpace(f[2]))
	return strings.TrimSpace(f[0]), dpm, err
}

// parseHHPrintWidth finds the PRINT WIDTH line of ^HH (the configuration label as text).
func parseHHPrintWidth(reply []byte) (int, bool) {
	for _, line := range strings.Split(string(reply), "\n") {
		f := strings.Fields(strings.Trim(line, "\x02\x03\r"))
		if len(f) == 3 && f[1] == "PRINT" && f[2] == "WIDTH" {
			n, err := strconv.Atoi(f[0])
			return n, err == nil
		}
	}
	return 0, false
}
