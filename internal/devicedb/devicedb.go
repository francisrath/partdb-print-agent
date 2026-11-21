// Package devicedb is pdb-print's built-in printer database: models (which driver, head, dpi,
// how to recognise them, quirks) and media (label stock geometry), as YAML embedded in the
// binary. Adding a model that an existing driver family can print is a data change only.
package devicedb

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
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

// Geometry is how a medium lies on one size of head, where that differs from the media entry:
// a DK roll sits on other pins of a 1296-pin QL than of a 720-pin one, and TZe tape has other dots
// on a 180 dpi P-touch. MediaFor applies it to models with that many head pins; every field
// replaces the media's (printable_length_dots only when set).
type Geometry struct {
	Media               string  `yaml:"media"`
	HeadPins            int     `yaml:"head_pins"`
	DPI                 int     `yaml:"dpi"`
	WidthDots           int     `yaml:"width_dots"`
	PrintableWidthDots  int     `yaml:"printable_width_dots"`
	PrintableLengthDots int     `yaml:"printable_length_dots"`
	OffsetPins          int     `yaml:"offset_pins"`
	EdgeMM              float64 `yaml:"edge_mm"`
}

type geoKey struct {
	media string
	pins  int
}

// Match says how a model recognises itself.
type Match struct {
	USB           []string `yaml:"usb"`            // "04f9:200f"
	IEEE1284Model string   `yaml:"ieee1284_model"` // MDL: value of the IEEE 1284 device ID
	StatusCode    *int     `yaml:"status_code"`    // model byte in the driver's status reply
	SNMPDescr     string   `yaml:"snmp_descr"`     // regexp on sysDescr
	HIModel       string   `yaml:"hi_model"`       // regexp on the model part of a Zebra ~HI reply
	BLEName       string   `yaml:"ble_name"`       // regexp on the BLE advertised name
	BTName        string   `yaml:"bt_name"`        // regexp on the Bluetooth Classic device name
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
	snmpRe     *regexp.Regexp    // Match.SNMPDescr, compiled at load
	hiRe       *regexp.Regexp    // Match.HIModel, compiled at load
	bleRe      *regexp.Regexp    // Match.BLEName, compiled at load
	btRe       *regexp.Regexp    // Match.BTName, compiled at load
	Defaults   map[string]string `yaml:"defaults"`
	// Tested says who printed with this model on real hardware: "maintainer", "community" or ""
	// (implemented from the documentation, not tested yet). TestedNote says how and when.
	Tested     string `yaml:"tested"`
	TestedNote string `yaml:"tested_note"`
}

// MatchesSNMP reports whether an SNMP device description names this model.
func (m *Model) MatchesSNMP(descr string) bool {
	return m.snmpRe != nil && descr != "" && m.snmpRe.MatchString(descr)
}

// Quirk returns an integer quirk, or def when the model doesn't set it.
func (m *Model) Quirk(key string, def int) int {
	if v, ok := m.Quirks[key].(int); ok {
		return v
	}
	return def
}

// QuirkBool reports a boolean quirk (false when unset).
func (m *Model) QuirkBool(key string) bool {
	b, _ := m.Quirks[key].(bool)
	return b
}

// QuirkUUID returns a GATT UUID quirk (ble_service, ble_write, ble_notify) as 128-bit text: a number is a
// 16-bit Bluetooth SIG UUID, a string a whole UUID (Load checks its form); def when unset or m is nil.
func (m *Model) QuirkUUID(key string, def uint16) string {
	if m != nil {
		if s, ok := m.Quirks[key].(string); ok {
			return strings.ToLower(s)
		}
		def = uint16(m.Quirk(key, int(def)))
	}
	return fmt.Sprintf("0000%04x-0000-1000-8000-00805f9b34fb", def)
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// QuirkString returns a string quirk, or "".
func (m *Model) QuirkString(key string) string {
	s, _ := m.Quirks[key].(string)
	return s
}

type DB struct {
	models   map[string]*Model
	media    map[string]Media
	geometry map[geoKey]Geometry
}

type file struct {
	Models   []*Model   `yaml:"models"`
	Media    []Media    `yaml:"media"`
	Geometry []Geometry `yaml:"geometry"`
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
	db := &DB{models: map[string]*Model{}, media: map[string]Media{}, geometry: map[geoKey]Geometry{}}
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
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			errs = append(errs, fmt.Errorf("%s: a data file must be one YAML document", base))
		}
		for _, m := range f.Media {
			if _, dup := db.media[m.ID]; dup || m.ID == "" {
				errs = append(errs, fmt.Errorf("%s: media %q missing or defined twice", base, m.ID))
			}
			db.media[m.ID] = m
		}
		for _, g := range f.Geometry {
			k := geoKey{g.Media, g.HeadPins}
			if _, dup := db.geometry[k]; dup || g.HeadPins <= 0 || g.DPI <= 0 || g.PrintableWidthDots <= 0 {
				errs = append(errs, fmt.Errorf("%s: geometry of %q on %d pins defined twice or without head_pins, dpi, printable_width_dots", base, g.Media, g.HeadPins))
			}
			db.geometry[k] = g
		}
		for i, m := range f.Models {
			if m == nil {
				errs = append(errs, fmt.Errorf("%s: models[%d] is an empty model", base, i))
				continue
			}
			if m.ID == "" || db.models[m.ID] != nil {
				errs = append(errs, fmt.Errorf("%s: model %q missing or defined twice", base, m.ID))
				continue
			}
			if m.Match.SNMPDescr != "" {
				re, err := regexp.Compile(m.Match.SNMPDescr)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: model %s: bad snmp_descr pattern: %w", base, m.ID, err))
				}
				m.snmpRe = re
			}
			if m.Match.HIModel != "" {
				re, err := regexp.Compile(m.Match.HIModel)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: model %s: bad hi_model pattern: %w", base, m.ID, err))
				}
				m.hiRe = re
			}
			if m.Match.BLEName != "" {
				re, err := regexp.Compile(m.Match.BLEName)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: model %s: bad ble_name pattern: %w", base, m.ID, err))
				}
				m.bleRe = re
			}
			if m.Match.BTName != "" {
				re, err := regexp.Compile(m.Match.BTName)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: model %s: bad bt_name pattern: %w", base, m.ID, err))
				}
				m.btRe = re
			}
			for _, k := range []string{"ble_service", "ble_write", "ble_notify"} {
				switch v := m.Quirks[k].(type) {
				case nil:
				case int:
					if v < 0 || v > 0xffff {
						errs = append(errs, fmt.Errorf("%s: model %s: %s %#x isn't a 16-bit UUID", base, m.ID, k, v))
					}
				case string:
					if !uuidRe.MatchString(v) {
						errs = append(errs, fmt.Errorf("%s: model %s: %s %q isn't a UUID", base, m.ID, k, v))
					}
				default:
					errs = append(errs, fmt.Errorf("%s: model %s: %s must be a 16-bit number or a UUID string", base, m.ID, k))
				}
			}
			if m.Driver == "" || m.Dialect == "" {
				errs = append(errs, fmt.Errorf("%s: model %s needs driver and dialect", base, m.ID))
			}
			if !slices.Contains([]string{"", "maintainer", "community"}, m.Tested) {
				errs = append(errs, fmt.Errorf("%s: model %s: tested is %q, want maintainer, community or empty", base, m.ID, m.Tested))
			}
			if m.TestedNote != "" && m.Tested == "" {
				errs = append(errs, fmt.Errorf("%s: model %s: tested_note needs tested", base, m.ID))
			}
			if (m.Driver == "escpos" || m.Driver == "tspl") && m.HeadPins <= 0 {
				errs = append(errs, fmt.Errorf("%s: model %s needs head_pins > 0", base, m.ID))
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
	for k := range db.geometry {
		if _, ok := db.media[k.media]; !ok {
			errs = append(errs, fmt.Errorf("geometry for unknown media %s", k.media))
		}
	}
	codes := map[string]string{} // family + status code → model id
	for _, id := range slices.Sorted(maps.Keys(db.models)) {
		m := db.models[id]
		if c := m.Match.StatusCode; c != nil {
			key := fmt.Sprintf("%s/0x%02x", m.Driver, *c)
			if other, dup := codes[key]; dup {
				errs = append(errs, fmt.Errorf("%s: models %s and %s both claim status code 0x%02x", origin[m.ID], other, m.ID, *c))
			}
			codes[key] = m.ID
		}
		for _, mid := range m.Media {
			if _, ok := db.media[mid]; !ok {
				errs = append(errs, fmt.Errorf("%s: model %s: unknown media %s", origin[m.ID], m.ID, mid))
				continue
			}
			// a geometry for this head that keeps the label's full width trims the edges instead of shrinking it
			raw := db.media[mid]
			g, geo := db.geometry[geoKey{mid, m.HeadPins}]
			geo = geo && max(g.WidthDots, g.PrintableWidthDots) >= dots(raw.WidthMM, g.DPI)
			if !geo && raw.DPI == 0 && m.DPI > 0 && m.HeadPins > 0 && dots(raw.WidthMM, m.DPI) > m.HeadPins {
				errs = append(errs, fmt.Errorf("%s: model %s: media %s is %d dots wide at %d dpi, head has %d: it would print narrower than its size",
					origin[m.ID], m.ID, mid, dots(raw.WidthMM, m.DPI), m.DPI, m.HeadPins))
			}
			md := db.on(m, mid)
			if m.HeadPins > 0 && md.OffsetPins+md.PrintableWidthDots > m.HeadPins {
				errs = append(errs, fmt.Errorf("%s: model %s: media %s needs pins %d–%d, head has %d", origin[m.ID], m.ID, mid,
					md.OffsetPins, md.OffsetPins+md.PrintableWidthDots-1, m.HeadPins))
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

// ModelIDs lists every model id, sorted.
func (db *DB) ModelIDs() []string {
	ids := make([]string, 0, len(db.models))
	for id := range db.models {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (db *DB) Media(id string) (Media, bool) {
	m, ok := db.media[id]
	return m, ok
}

// MediaFor lists the media a model can print, in the model's order.
// Media given in mm only (dpi 0, e.g. the die-cut label catalogue) get the model's dots.
func (db *DB) MediaFor(m *Model) []Media {
	out := make([]Media, 0, len(m.Media))
	for _, id := range m.Media {
		out = append(out, db.on(m, id))
	}
	return out
}

// on is media id as model m prints it: its head's geometry, or dots from mm at the model's dpi.
func (db *DB) on(m *Model, id string) Media {
	md := db.media[id]
	if g, ok := db.geometry[geoKey{id, m.HeadPins}]; ok {
		md.DPI, md.WidthDots, md.PrintableWidthDots, md.OffsetPins, md.EdgeMM = g.DPI, g.WidthDots, g.PrintableWidthDots, g.OffsetPins, g.EdgeMM
		if g.PrintableLengthDots > 0 {
			md.PrintableLengthDots = g.PrintableLengthDots
		}
	}
	if md.DPI == 0 && m.DPI > 0 {
		md.DPI = m.DPI
		md.PrintableWidthDots = dots(md.WidthMM, m.DPI)
		if m.HeadPins > 0 {
			md.PrintableWidthDots = min(md.PrintableWidthDots, m.HeadPins)
		}
		md.PrintableLengthDots = dots(md.LengthMM, m.DPI)
	}
	return md
}

// dots converts mm at whole dots per mm: printers sold as "203/300 dpi" in this catalogue (Zebra)
// have 8 and 12 dots/mm heads (~HI reports dots/mm), i.e. 203.2 and 304.8 dpi.
func dots(mm float64, dpi int) int {
	return int(math.Round(mm * math.Round(float64(dpi)/25.4)))
}

// ModelByZebraMDL finds a Zebra from the MODEL of its IEEE 1284 device ID on USB: "ZTC GK420d" or
// "ZTC ZD420-300dpi ZPL" (the dpi, when there, picks the variant; else the model's first one).
func (db *DB) ModelByZebraMDL(mdl string) (*Model, bool) {
	name, ok := strings.CutPrefix(strings.TrimSpace(mdl), "ZTC ")
	if !ok {
		return nil, false
	}
	// the whole name ("ZP 450" has a space), without a trailing language token
	name = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), " ZPL"))
	dpi := 0
	if i := strings.LastIndex(name, "-"); i > 0 && strings.HasSuffix(name, "dpi") {
		dpi, _ = strconv.Atoi(strings.TrimSuffix(name[i+1:], "dpi"))
		dpi = map[int]int{200: 203, 203: 203, 300: 300, 600: 600}[dpi]
	}
	for _, id := range db.ModelIDs() {
		m := db.models[id]
		if m.hiRe != nil && m.hiRe.MatchString(name) && (dpi == 0 || m.DPI == dpi) {
			return m, true
		}
	}
	return nil, false
}

// ModelByHI finds a model from a Zebra ~HI reply ("GK420d-200dpi,V61.17.11Z,8,2104KB"): the model
// part matches hi_model, and the dots per mm (third field: 8, 12, 24) pick the dpi variant.
func (db *DB) ModelByHI(hi string) (*Model, bool) {
	hi = strings.Trim(strings.TrimSpace(hi), "\x02\x03\r\n ")
	f := strings.Split(hi, ",")
	if len(f) < 3 {
		return nil, false
	}
	dpi := map[string]int{"6": 152, "8": 203, "12": 300, "24": 600}[strings.TrimSpace(f[2])]
	for _, id := range db.ModelIDs() {
		m := db.models[id]
		if m.hiRe != nil && m.hiRe.MatchString(f[0]) && m.DPI == dpi {
			return m, true
		}
	}
	return nil, false
}

// ModelByBLEName finds a model from the name a Bluetooth LE printer advertises ("MX10").
// The longest match wins, so "B1-PRO-…" picks the B1-PRO over the B1 ("B1-…"); ties go to the first id.
func (db *DB) ModelByBLEName(name string) (*Model, bool) {
	var best *Model
	n := -1
	for _, id := range db.ModelIDs() {
		m := db.models[id]
		if m.bleRe == nil || name == "" {
			continue
		}
		if loc := m.bleRe.FindStringIndex(name); loc != nil && loc[1]-loc[0] > n {
			best, n = m, loc[1]-loc[0]
		}
	}
	return best, best != nil
}

// ModelByBTName finds a model from the name a Bluetooth Classic printer is paired as ("RG-MLP80B").
func (db *DB) ModelByBTName(name string) (*Model, bool) {
	for _, id := range db.ModelIDs() {
		m := db.models[id]
		if m.btRe != nil && name != "" && m.btRe.MatchString(name) {
			return m, true
		}
	}
	return nil, false
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
