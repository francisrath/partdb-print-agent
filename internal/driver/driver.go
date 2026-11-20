// Package driver defines the printer driver and transport contracts (architecture §7).
package driver

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"sort"
	"sync"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
)

// Driver prints on one printer of a family. It is built per printer from its device-database
// model and options, and owns the whole print flow after rendering (encode, send, pace, confirm).
type Driver interface {
	// Status reads the printer's state; ErrNoReadback if it can't say anything.
	Status(ctx context.Context, t Transport) (*Status, error)
	// Print validates, encodes and sends job, then waits for the printer to confirm where it can.
	Print(ctx context.Context, t Transport, job *Job) (*Result, error)
	// Encode writes the same byte stream Print would send (for --dry-run).
	Encode(w io.Writer, job *Job) error
}

// Options are per-printer settings a driver may need besides the transport.
type Options struct {
	Host          string // network printers: for side channels such as SNMP
	SNMPCommunity string
	NoSNMP        bool
	// zpl: move the print (dots; x > 0 right, y > 0 down) or widen it (0 = the media's width).
	// Set by `pdb-print try` while it looks for what fixes a cut-off label.
	OffsetXDots, OffsetYDots, PrintWidthDots int
}

// Result is what a print did.
type Result struct {
	Pages     int
	Media     string
	MediaFrom string // where the media came from: detected, config fallback (set by printing)
	Bytes     int
	Confirmed bool   // the printer reported completion
	Message   string // e.g. "printed", "sent (unconfirmed: …)"
	Warnings  []string
}

var (
	regMu    sync.Mutex
	families = map[string]func(*devicedb.Model, Options) (Driver, error){}
)

// Register makes a driver family available; call it from the family package's init.
func Register(family string, newDriver func(*devicedb.Model, Options) (Driver, error)) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := families[family]; dup {
		panic("driver: family registered twice: " + family)
	}
	families[family] = newDriver
}

// New builds the driver for a model.
func New(m *devicedb.Model, o Options) (Driver, error) {
	regMu.Lock()
	newDriver, ok := families[m.Driver]
	regMu.Unlock()
	if !ok {
		return nil, &Error{Code: Unsupported, Detail: fmt.Sprintf("no driver %q for %s", m.Driver, m.ID)}
	}
	return newDriver(m, o)
}

// Families lists the registered driver families, sorted.
func Families() []string {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]string, 0, len(families))
	for f := range families {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Transport is a byte pipe with optional read-back.
type Transport interface {
	Open(ctx context.Context) error
	Write(p []byte) (int, error)
	Read(p []byte) (int, error) // ErrNoReadback (or a timeout) if nothing comes back
	Close() error
	Kind() string
}

var ErrNoReadback = errors.New("transport has no read-back")

// Media and its kinds live in the device database; these aliases keep driver.Media working.
type (
	MediaKind = devicedb.MediaKind
	Media     = devicedb.Media
)

const (
	Continuous = devicedb.Continuous
	DieCut     = devicedb.DieCut
)

type Capabilities struct {
	DPI      int
	HeadDots int
	Cutter   bool
	Media    []Media
}

type DeviceInfo struct {
	Model string
	Media *Media
}

// Job is one print job after rendering.
type Job struct {
	Pages  []*image.Gray // 1 dot per pixel, black < 128, rows in feed order
	Media  *Media
	Cut    string  // "" = the model's default
	Status *Status // the status read before printing; nil if the printer gave none
}

// Code is a normalised status code (architecture §9).
type Code string

const (
	OK            Code = "OK"
	Busy          Code = "BUSY"
	NoMedia       Code = "NO_MEDIA"
	MediaMismatch Code = "MEDIA_MISMATCH"
	MediaEndSoon  Code = "MEDIA_END_SOON"
	CoverOpen     Code = "COVER_OPEN"
	CutterJam     Code = "CUTTER_JAM"
	PaperJam      Code = "PAPER_JAM"
	Overheat      Code = "OVERHEAT"
	Paused        Code = "PAUSED"     // the printer is paused (Zebra: press the feed button)
	RibbonOut     Code = "RIBBON_OUT" // thermal transfer printers
	CommsError    Code = "COMMS_ERROR"
	Unsupported   Code = "UNSUPPORTED"
	RenderError   Code = "RENDER_ERROR"
	Unknown       Code = "UNKNOWN"
)

type Status struct {
	Code   Code
	Detail string
	Model  string
	Media  *Media // loaded media, if the printer reports it
	// MediaType is the tape kind where the printer reports it (brother-pt: laminated,
	// non-laminated, lettering, hg, av); "" otherwise.
	MediaType string
	Done      bool   // a "printing completed" notification
	Idle      bool   // idle when read (QL over SNMP: confirms a job only if it was idle before)
	Via       string // where the status came from: the driver's own protocol ("" ), or e.g. "snmp", "tcp"
}

// Error is a printer-side failure with a normalised code.
type Error struct {
	Code   Code
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}
