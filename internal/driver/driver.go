// Package driver defines the printer driver and transport contracts (architecture §7).
package driver

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
)

// Driver turns rendered pages into a byte stream for one printer family.
type Driver interface {
	ID() string
	Capabilities(model string) (Capabilities, error)
	// Probe identifies the model and loaded media (nil fields when unknown).
	Probe(ctx context.Context, t Transport) (*DeviceInfo, error)
	Validate(pages []*image.Gray, job *Job, media *Media) error
	// Encode writes the job. Pages are 1 dot per pixel, black < 128, rows in feed order.
	// ponytail: *image.Gray is mono only; two-colour (phase 2) needs a Raster with planes.
	Encode(ctx context.Context, w io.Writer, pages []*image.Gray, job *Job, media *Media) error
	// Status reads the current printer state; returns ErrNoReadback if the transport can't.
	Status(ctx context.Context, t Transport) (*Status, error)
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

type MediaKind string

const (
	Continuous MediaKind = "continuous"
	DieCut     MediaKind = "die_cut"
)

// Media is a label stock as the renderer and driver see it.
type Media struct {
	ID                  string
	Name                string
	Kind                MediaKind
	WidthMM, LengthMM   float64 // LengthMM = 0 for continuous
	PrintableWidthDots  int     // cross-feed
	PrintableLengthDots int     // feed; 0 for continuous
	DPI                 int
}

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

// Job holds per-job options; zero value = driver defaults.
type Job struct {
	Cut string // per_label (default) | last | none
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
	Done   bool   // a "printing completed" notification
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
