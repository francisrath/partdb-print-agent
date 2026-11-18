// Package devicedb is pdb-print's built-in printer database: models (which driver, head, dpi,
// how to recognise them, quirks) and media (label stock geometry), as YAML embedded in the
// binary. Adding a model that an existing driver family can print is a data change only.
package devicedb

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
)

type MediaKind string

const (
	Continuous MediaKind = "continuous"
	DieCut     MediaKind = "die_cut"
)

// Media is a label stock as the renderer and driver see it.
type Media struct {
	ID                 string    `yaml:"id"`
	Name               string    `yaml:"name"`
	Kind               MediaKind `yaml:"kind"`
	WidthMM            float64   `yaml:"width_mm"`
	LengthMM           float64   `yaml:"length_mm"` // 0 for continuous
	PrintableWidthDots int       `yaml:"printable_width_dots"`
	// WidthDots is the whole media width in dots when wider than the printable area (tape:
	// labels are drawn across the full tape and the unprintable edges trimmed); 0 = same.
	WidthDots           int `yaml:"width_dots"`
	PrintableLengthDots int `yaml:"printable_length_dots"` // 0 for continuous
	DPI                 int `yaml:"dpi"`
	// OffsetPins are the unprinted head pins before the printable area.
	OffsetPins int `yaml:"offset_pins"`
	// EdgeMM is the unprintable edge per side across the media (guidance for label design).
	EdgeMM float64 `yaml:"edge_mm"`
}

// Match says how a model recognises itself.
type Match struct {
	USB           []string `yaml:"usb"`            // "04f9:200f"
	IEEE1284Model string   `yaml:"ieee1284_model"` // MDL: value of the IEEE 1284 device ID
	StatusCode    *int     `yaml:"status_code"`    // model byte in the driver's status reply
	SNMPDescr     string   `yaml:"snmp_descr"`     // regexp on sysDescr
}

type Model struct {
	ID         string            `yaml:"id"`
	Vendor     string            `yaml:"vendor"`
	Driver     string            `yaml:"driver"`  // driver family
	Dialect    string            `yaml:"dialect"` // driver-specific variant
	DPI        int               `yaml:"dpi"`
	HeadPins   int               `yaml:"head_pins"`
	Transports []string          `yaml:"transports"`
	Match      Match             `yaml:"match"`
	Media      []string          `yaml:"media"`
	Quirks     map[string]any    `yaml:"quirks"`
	Defaults   map[string]string `yaml:"defaults"`
}

// Quirk returns an integer quirk, or def when the model doesn't set it.
func (m *Model) Quirk(key string, def int) int {
	if v, ok := m.Quirks[key].(int); ok {
		return v
	}
	return def
}

// QuirkString returns a string quirk, or "".
func (m *Model) QuirkString(key string) string {
	s, _ := m.Quirks[key].(string)
	return s
}

type DB struct {
	models map[string]*Model
	media  map[string]Media
}

type file struct {
	Models []*Model `yaml:"models"`
	Media  []Media  `yaml:"media"`
}

//go:embed data/*.yaml
var data embed.FS

var (
	once sync.Once
	def  *DB
)

// Default is the embedded database. TestLint and TestDefaultLoads keep it loadable.
func Default() *DB {
	once.Do(func() {
		db, err := Load(data)
		if err != nil {
			panic("devicedb: embedded data: " + err.Error())
		}
		def = db
	})
	return def
}

// Load reads every data/*.yaml in fsys and checks references.
func Load(fsys fs.FS) (*DB, error) {
	names, err := fs.Glob(fsys, "data/*.yaml")
	if err != nil {
		return nil, err
	}
	db := &DB{models: map[string]*Model{}, media: map[string]Media{}}
	origin := map[string]string{} // model id → file
	usb := map[string]string{}    // usb id → model id
	var errs []error
	sort.Strings(names)
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		var f file
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		base := path.Base(n)
		for _, m := range f.Media {
			if _, dup := db.media[m.ID]; dup || m.ID == "" {
				errs = append(errs, fmt.Errorf("%s: media %q missing or defined twice", base, m.ID))
			}
			db.media[m.ID] = m
		}
		for _, m := range f.Models {
			if m.ID == "" || db.models[m.ID] != nil {
				errs = append(errs, fmt.Errorf("%s: model %q missing or defined twice", base, m.ID))
				continue
			}
			if m.Driver == "" || m.Dialect == "" {
				errs = append(errs, fmt.Errorf("%s: model %s needs driver and dialect", base, m.ID))
			}
			for _, u := range m.Match.USB {
				if other, dup := usb[u]; dup {
					errs = append(errs, fmt.Errorf("%s: model %s: usb %s already used by %s (%s)", base, m.ID, u, other, origin[other]))
				}
				usb[u] = m.ID
			}
			db.models[m.ID], origin[m.ID] = m, base
		}
	}
	for _, m := range db.models {
		for _, id := range m.Media {
			if _, ok := db.media[id]; !ok {
				errs = append(errs, fmt.Errorf("%s: model %s: unknown media %s", origin[m.ID], m.ID, id))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return db, nil
}

func (db *DB) Model(id string) (*Model, bool) {
	m, ok := db.models[id]
	return m, ok
}

// Models lists a driver family's models, sorted by id.
func (db *DB) Models(driver string) []*Model {
	var out []*Model
	for _, m := range db.models {
		if m.Driver == driver {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b *Model) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func (db *DB) Media(id string) (Media, bool) {
	m, ok := db.media[id]
	return m, ok
}

// MediaFor lists the media a model can print, in the model's order.
func (db *DB) MediaFor(m *Model) []Media {
	out := make([]Media, 0, len(m.Media))
	for _, id := range m.Media {
		out = append(out, db.media[id])
	}
	return out
}

// ModelByStatusCode finds a family's model by the model byte its status reply carries.
func (db *DB) ModelByStatusCode(driver string, code int) (*Model, bool) {
	for _, m := range db.Models(driver) {
		if m.Match.StatusCode != nil && *m.Match.StatusCode == code {
			return m, true
		}
	}
	return nil, false
}
