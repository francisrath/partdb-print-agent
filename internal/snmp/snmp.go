// Package snmp reads printer state over SNMP (standard Host-Resources and Printer MIBs).
// Network Brother QLs never answer status on port 9100, but they do report the loaded
// roll, errors and printing progress here.
package snmp

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/gosnmp/gosnmp"
)

const (
	oidDeviceDescr  = "1.3.6.1.2.1.25.3.2.1.3.1"    // hrDeviceDescr
	oidDeviceStatus = "1.3.6.1.2.1.25.3.2.1.5.1"    // hrDeviceStatus: running(2) warning(3) testing(4) down(5)
	oidPrinterState = "1.3.6.1.2.1.25.3.5.1.1.1"    // hrPrinterStatus: other(1) unknown(2) idle(3) printing(4) warmup(5)
	oidDisplay      = "1.3.6.1.2.1.43.16.5.1.2.1.1" // prtConsoleDisplayBufferText
	oidMediaName    = "1.3.6.1.2.1.43.8.2.1.12.1.1" // prtInputMediaName
	oidAlertCodes   = "1.3.6.1.2.1.43.18.1.1.7"     // prtAlertCode (table)
)

// State is one SNMP snapshot of a printer.
type State struct {
	Model     string
	Down      bool // hrDeviceStatus = down(5)
	Printing  bool // hrPrinterStatus printing(4), or other(1) = BUSY while cutting
	Idle      bool // hrPrinterStatus idle(3)
	Display   string
	MediaName string // e.g. `29mm / 1.1"`
	Alerts    []int  // prtAlertCode values, 0s dropped
}

// Query reads the printer's state. Timeout applies per request.
func Query(ctx context.Context, host, community string, timeout time.Duration) (*State, error) {
	g := &gosnmp.GoSNMP{Target: host, Port: 161, Community: community, Version: gosnmp.Version2c,
		Timeout: timeout, Retries: 0, Context: ctx}
	if err := g.Connect(); err != nil {
		return nil, err
	}
	defer g.Conn.Close()

	res, err := g.Get([]string{oidDeviceDescr, oidDeviceStatus, oidPrinterState, oidDisplay, oidMediaName})
	if err != nil {
		return nil, fmt.Errorf("snmp %s: %w", host, err)
	}
	s := &State{}
	for _, v := range res.Variables {
		switch strings.TrimPrefix(v.Name, ".") {
		case oidDeviceDescr:
			s.Model = str(v)
		case oidDeviceStatus:
			s.Down = num(v) == 5
		case oidPrinterState:
			s.Idle, s.Printing = num(v) == 3, num(v) == 4 || num(v) == 1
		case oidDisplay:
			s.Display = strings.TrimSpace(str(v))
		case oidMediaName:
			s.MediaName = str(v)
		}
	}
	if s.Model == "" {
		return nil, fmt.Errorf("snmp %s: no printer data (wrong community?)", host)
	}
	// GETNEXT walk: the QL-810W doesn't answer GETBULK
	err = g.Walk(oidAlertCodes, func(v gosnmp.SnmpPDU) error {
		if c := num(v); c != 0 {
			s.Alerts = append(s.Alerts, c)
		}
		return nil
	})
	return s, err
}

func str(v gosnmp.SnmpPDU) string {
	if b, ok := v.Value.([]byte); ok {
		return string(b)
	}
	return ""
}

func num(v gosnmp.SnmpPDU) int { return int(gosnmp.ToBigInt(v.Value).Int64()) }

// alertCodes maps IANA PrtAlertCodeTC values to normalised codes.
var alertCodes = map[int]struct {
	code   driver.Code
	detail string
}{
	3: {driver.CoverOpen, "cover open"}, 501: {driver.CoverOpen, "cover open"},
	8: {driver.PaperJam, "jam"}, 815: {driver.PaperJam, "media jam"},
	802: {driver.MediaMismatch, "wrong media loaded for this job (clear the error on the printer)"},
	804: {driver.MediaMismatch, "wrong media type loaded"},
	809: {driver.MediaMismatch, "printer asks for a media change"},
	808: {driver.NoMedia, "media empty"},
	807: {driver.MediaEndSoon, "media low"},
}

// Problem returns why the printer can't print now, or nil.
func (s *State) Problem() *driver.Error {
	for _, a := range s.Alerts {
		if m, ok := alertCodes[a]; ok && m.code != driver.MediaEndSoon {
			return &driver.Error{Code: m.code, Detail: m.detail}
		}
	}
	if s.Down {
		return &driver.Error{Code: driver.Unknown, Detail: fmt.Sprintf("printer error (%s, alerts %v)", s.Display, s.Alerts)}
	}
	return nil
}

// e.g. `29mm / 1.1"`; die-cut names assumed like `17mm x 54mm …`
var mediaRe = regexp.MustCompile(`^\s*(\d+)\s*mm(?:\s*[x×]\s*(\d+)\s*mm)?`)

// LoadedMedia parses the roll the printer reports into kind and size in mm.
// ponytail: only "29mm / 1.1\"" (DK-22210) is verified on hardware; other rolls
// fall back to the configured media until their names are confirmed.
func (s *State) LoadedMedia() (kind driver.MediaKind, widthMM, lengthMM int, ok bool) {
	m := mediaRe.FindStringSubmatch(s.MediaName)
	if m == nil {
		return "", 0, 0, false
	}
	widthMM, _ = strconv.Atoi(m[1])
	if m[2] == "" {
		return driver.Continuous, widthMM, 0, true
	}
	lengthMM, _ = strconv.Atoi(m[2])
	return driver.DieCut, widthMM, lengthMM, true
}
