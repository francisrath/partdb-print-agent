// Package try is `pdb-print try`: a guided test of a printer by someone standing next to it. It
// asks before sending anything, prints numbered test labels, asks what came out, tries the fix
// for a "no" in the same sitting, puts the printer's settings back and writes one report file.
package try

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/discovery"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/pdfgen"
	"github.com/francisrath/partdb-print-agent/internal/printing"
	"github.com/francisrath/partdb-print-agent/internal/report"
	"github.com/francisrath/partdb-print-agent/internal/transport"
)

// Options for Run. Discover, Report and Now are for tests (nil = the real ones).
type Options struct {
	In       io.Reader
	Out      io.Writer
	Host     string // the printer's address; "" = search the network
	Port     int    // 0 = 9100
	Dir      string // where the report file goes; "" = the current directory
	Svc      *printing.Service
	DB       *devicedb.DB
	Version  string
	Discover func(ctx context.Context) ([]discovery.Candidate, error)
	Now      func() time.Time
}

type session struct {
	o         Options
	ctx       context.Context
	lines     chan string
	stopped   bool // input ended or the user quit: finish up
	rep       *report.Report
	sum       []string // the human summary at the top of the file
	printer   *config.Printer
	model     *devicedb.Model
	candidate discovery.Candidate
	setup     map[string]int // the printer's own ^PW, ^LL, ^LH x/y, ^LS from ^HZA/^HH/~HS
	printed   bool           // a job ran: settings must be put back
}

// Run runs the session; it always writes the report (also when stopped early) and returns its path.
func Run(ctx context.Context, o Options) (string, error) {
	if o.DB == nil {
		o.DB = devicedb.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Port == 0 {
		o.Port = 9100
	}
	s := &session{o: o, ctx: ctx, lines: make(chan string), setup: map[string]int{},
		rep: &report.Report{Tool: "pdb-print " + o.Version, OS: runtime.GOOS + "/" + runtime.GOARCH, Time: o.Now().UTC().Format(time.RFC3339)}}
	go func() { // stdin can't be interrupted: read it in the background
		sc := bufio.NewScanner(o.In)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
		close(s.lines)
	}()
	s.say("pdb-print printer test %s\n", o.Version)
	s.say("Answer each question with y (yes) or n (no) and Enter. Enter alone takes the answer in capitals.\n\n")
	s.run()
	s.restore()
	return s.write()
}

func (s *session) say(format string, a ...any) { fmt.Fprintf(s.o.Out, format, a...) }

func (s *session) note(format string, a ...any) { s.sum = append(s.sum, fmt.Sprintf(format, a...)) }

// line reads one answer; "" and stopped when input ends or ctx is cancelled.
func (s *session) line() string {
	if s.stopped {
		return ""
	}
	select {
	case l, ok := <-s.lines:
		if !ok {
			s.stopped = true
		}
		return strings.TrimSpace(l)
	case <-s.ctx.Done():
		s.stopped = true
		return ""
	}
}

// ask is a yes/no question; Enter takes def, anything else asks again.
func (s *session) ask(q string, def bool) bool {
	hint := " (y/N) "
	if def {
		hint = " (Y/n) "
	}
	for {
		s.say("%s%s", q, hint)
		a := strings.ToLower(s.line())
		switch {
		case s.stopped:
			s.say("\n")
			return def
		case a == "":
			s.note("  %s → %s (Enter)", q, yn(def))
			return def
		case slices.Contains([]string{"y", "yes", "j", "ja"}, a):
			s.note("  %s → yes", q)
			return true
		case slices.Contains([]string{"n", "no", "nei", "nein"}, a):
			s.note("  %s → no", q)
			return false
		}
		s.say("Please answer y or n.\n")
	}
}

// choose asks for one of the letters in opts ("t", "b", …); "" = skip.
func (s *session) choose(q string, opts ...string) string {
	for {
		s.say("%s ", q)
		a := strings.ToLower(s.line())
		if s.stopped || a == "" || slices.Contains(opts, a) {
			s.note("  %s → %q", q, a)
			return a
		}
		s.say("Please type one of %s, or just Enter.\n", strings.Join(opts, ", "))
	}
}

// mm asks for a length in mm; false when the user doesn't know.
func (s *session) mm(q string) (float64, bool) {
	for {
		s.say("%s ", q)
		a := strings.ReplaceAll(s.line(), ",", ".")
		if s.stopped || a == "" {
			s.note("  %s → (don't know)", q)
			return 0, false
		}
		if v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(a), "mm"), 64); err == nil && v >= 0 && v < 50 {
			s.note("  %s → %.1f mm", q, v)
			return v, true
		}
		s.say("Please type a number of millimetres, like 3, or just Enter if you can't tell.\n")
	}
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func (s *session) run() {
	c, ok := s.find()
	if !ok || s.stopped {
		return
	}
	if !s.identify(c) || s.stopped {
		return
	}
	if !s.pickMedia() || s.stopped {
		return
	}
	labels := 6 // 5 test labels; closing the lid at step 5 may feed one more
	if s.square() {
		labels = 5
	}
	if !s.ask(fmt.Sprintf("Print the test labels now? This uses %d labels (more if we try fixes).", labels), false) {
		s.note("Stopped before printing.")
		return
	}
	s.card1()
	for _, step := range []func(){s.card2, s.card3, s.card4, s.card5} {
		if s.stopped || s.ctx.Err() != nil {
			return
		}
		step()
	}
}

// find picks the printer: the given address, else what discovery finds, else asks for an address.
func (s *session) find() (discovery.Candidate, bool) {
	cand := func(host string) discovery.Candidate {
		return discovery.Candidate{Kind: "tcp", Host: host, Port: s.o.Port, Status: discovery.Unknown}
	}
	if s.o.Host != "" {
		return cand(s.o.Host), true
	}
	s.say("Looking for printers on your network (about 10 seconds) …\n")
	discover := s.o.Discover
	if discover == nil {
		discover = func(ctx context.Context) ([]discovery.Candidate, error) {
			usb, _ := discovery.USB(s.o.DB, "")
			lan, err := discovery.Network(ctx, s.o.DB, discovery.Options{Timeout: 15 * time.Second, Port: s.o.Port})
			return append(usb, lan...), err
		}
	}
	cs, err := discover(s.ctx)
	if err != nil {
		s.rep.Notes = append(s.rep.Notes, "discovery: "+err.Error())
	}
	s.rep.Candidates = cs
	switch len(cs) {
	case 0:
		s.say("No printer answered. ")
		for {
			s.say("Type the printer's IP address (it is on the printer's network settings label), or Enter to stop: ")
			a := s.line()
			if s.stopped || a == "" {
				s.note("No printer found.")
				return discovery.Candidate{}, false
			}
			if ip := net.ParseIP(a); ip != nil {
				return cand(a), true
			}
			s.say("That isn't an IP address like 192.168.1.50.\n")
		}
	case 1:
		s.say("Found a printer at %s.\n", where(cs[0]))
		return cs[0], true
	}
	for i, c := range cs {
		name := c.ModelID
		if name == "" {
			name = "(doesn't say what it is: could be the Zebra)"
		}
		s.say("  %d) %s %s\n", i+1, where(c), name)
	}
	for {
		s.say("Which one is the printer to test? (1–%d) ", len(cs))
		a := s.line()
		if s.stopped {
			return discovery.Candidate{}, false
		}
		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(cs) {
			return cs[n-1], true
		}
		s.say("Please type one of the numbers in the list.\n")
	}
}

func where(c discovery.Candidate) string {
	if c.Kind == "usb" {
		return "USB " + c.Device
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// identify asks the printer what it is (after a yes) and reads its setup (read-only queries).
func (s *session) identify(c discovery.Candidate) bool {
	if c.Kind == "tcp" && c.Status != discovery.Supported {
		if !s.ask("Ask this printer what it is? This sends 3 characters (~HI); a printer that isn't a Zebra may print them on a label.", false) {
			s.note("Stopped before asking the printer anything.")
			return false
		}
		ic, reply, err := discovery.Identify(s.ctx, s.o.DB, c)
		s.rep.Queries = append(s.rep.Queries, report.Query{Target: where(c), Name: "zpl ~HI", Sent: "~HI\\r\\n", Reply: report.Escape([]byte(reply))})
		if err != nil {
			s.say("\n%v\n", err)
			s.note("Identify: %v", err)
			if strings.Contains(err.Error(), "EPL") {
				s.say("If this is a Zebra, it may be set to its older language (EPL). Its manual's \"device.languages\" setting switches it to ZPL; pdb-print doesn't change it for you.\n")
			}
			return false
		}
		c = ic
	}
	if c.Model == nil || c.Model.Driver != "zpl" {
		s.say("pdb-print try tests Zebra printers for now; this one is %q.\n", c.ModelID)
		s.note("Not a Zebra: %q", c.ModelID)
		return false
	}
	s.model, s.candidate = c.Model, c
	// the read-only queries, recorded with their replies (it answered ~HI: it's a Zebra)
	s.rep.ZebraQueries(s.ctx, c, where(c))
	s.readSetup()
	s.printer = &config.Printer{ID: "try", Name: "test", Model: c.Model.ID,
		Transport: config.Transport{Kind: c.Kind, Host: c.Host, Port: c.Port, Device: c.Device}}
	s.say("This is a Zebra %s (%d dpi).\n", c.Model.ID, c.Model.DPI)
	s.note("Printer: Zebra %s, %d dpi; print width %d dots, label length %d dots (as the printer reports)",
		c.Model.ID, c.Model.DPI, s.setup["pw"], s.setup["ll"])
	return true
}

// setupRes read the printer's own page settings from ^HH (print width, label length) and ^HZA
// (shift, home, top), as a GK420d with firmware V61 writes them. Replies are recorded escaped
// (\r\n as text), hence the (?:\\[rn]|\s)* between tags.
var setupRes = map[string]*regexp.Regexp{
	"pw":  regexp.MustCompile(`(\d+)\s+PRINT WIDTH`),
	"ll":  regexp.MustCompile(`(\d+)\s+LABEL LENGTH`),
	"ls":  regexp.MustCompile(`<LABEL-SHIFT[^>]*>(-?\d+)</LABEL-SHIFT>`),
	"lhx": regexp.MustCompile(`<LABEL-HOME>(?:\\[rn]|\s)*<X-AXIS[^>]*>(-?\d+)</X-AXIS>`),
	"lhy": regexp.MustCompile(`<Y-AXIS[^>]*>(-?\d+)</Y-AXIS>`),
	"lt":  regexp.MustCompile(`<LABEL-TOP[^>]*>(?:\\[rn]|\s)*<CURRENT>(-?\d+)</CURRENT>`),
}

// readSetup takes the printer's own settings from the recorded ^HH and ^HZA replies.
func (s *session) readSetup() {
	for _, q := range s.rep.Queries {
		for k, re := range setupRes {
			if m := re.FindStringSubmatch(q.Reply); m != nil {
				if _, done := s.setup[k]; !done {
					s.setup[k], _ = strconv.Atoi(m[1])
				}
			}
		}
	}
}

func (s *session) dpm() float64 { return math.Round(float64(s.model.DPI) / 25.4) }

// pickMedia asks which labels are loaded, suggesting the size nearest to what the printer reports.
func (s *session) pickMedia() bool {
	media := s.o.DB.MediaFor(s.model)
	best := 0
	if pw, ll := float64(s.setup["pw"]), float64(s.setup["ll"]); pw > 0 && ll > 0 {
		d := math.Inf(1)
		for i, m := range media {
			if v := math.Abs(m.WidthMM-pw/s.dpm()) + math.Abs(m.LengthMM-ll/s.dpm()); v < d {
				best, d = i, v
			}
		}
	}
	s.say("\nWhich labels are loaded? Measure one label if you're not sure.\n")
	for i, m := range media {
		s.say("  %2d) %s\n", i+1, m.Name)
	}
	for {
		s.say("Number, or Enter for %d (%s): ", best+1, media[best].Name)
		a := s.line()
		if s.stopped {
			return false
		}
		n := best + 1
		if a != "" {
			v, err := strconv.Atoi(a)
			if err != nil || v < 1 || v > len(media) {
				s.say("Please type a number from the list.\n")
				continue
			}
			n = v
		}
		s.printer.Media = media[n-1].ID
		s.note("Labels: %s", media[n-1].Name)
		return true
	}
}

func (s *session) square() bool {
	m, _ := printing.LookupMedia(s.printer, s.printer.Media)
	return math.Abs(m.WidthMM-m.LengthMM) < 1
}

// print sends a job and records what happened; false when nothing could be printed.
func (s *session) print(tag string, src printing.Source) bool {
	s.say("Printing label %s …\n", tag)
	start := time.Now()
	res, err := s.o.Svc.Print(s.ctx, s.printer, "", src)
	s.printed = true
	q := report.Query{Target: s.printer.Transport.Host, Name: "print " + tag, TookMS: time.Since(start).Milliseconds()}
	if p := s.printer; p.Mirror || p.UpsideDown || p.Adjust != (config.Adjust{}) {
		q.Sent = fmt.Sprintf("mirror=%v upside_down=%v adjust=%+v", p.Mirror, p.UpsideDown, p.Adjust)
	}
	if err != nil {
		q.Err = err.Error()
		s.say("It didn't print: %v\n", err)
		s.note("Label %s: not printed: %v", tag, err)
	} else {
		q.Reply = fmt.Sprintf("%+v", *res)
		s.note("Label %s: %s (%d bytes, %.1f s)", tag, res.Message, res.Bytes, time.Since(start).Seconds())
	}
	s.rep.Queries = append(s.rep.Queries, q)
	return err == nil
}

// card1 is the test card, with the fixes for what can go wrong with it.
func (s *session) card1() {
	s.say("\n— Label 1/5: test card —\n")
	src := func(tag string) printing.Source {
		return printing.TestCardTagged(s.printer.Model, s.o.Version, tag)
	}
	if !s.print("1/5", src("1/5")) {
		s.status("after label 1")
		return
	}
	retried := false
	if !s.ask("Did label 1/5 come out of the printer?", true) {
		s.status("nothing came out")
		s.rep.ZebraQueries(s.ctx, s.candidate, where(s.candidate)) // buffer counters, errors, setup
		s.ask("Is a light on the printer flashing red or orange?", false)
		if !s.ask("Try printing it once more?", false) || !s.print("1b/5", src("1b/5")) || !s.ask("Did label 1b/5 come out?", false) {
			return
		}
		retried = true
	}
	retry := 'b'
	if retried {
		retry = 'c'
	}
	again := func() bool {
		retry++
		tag := fmt.Sprintf("1%c/5", retry-1)
		return s.print(tag, src(tag))
	}
	for range 3 {
		if s.stopped || s.ask("Is the whole frame (the border line) visible on all four sides?", true) {
			break
		}
		side := s.choose("Which side is cut off or missing? t = top, b = bottom, l = left, r = right (Enter = skip):", "t", "b", "l", "r")
		if side == "" {
			break
		}
		q := "About how many mm are missing there? (Enter if you can't tell)"
		if side == "t" || side == "l" {
			q = "About how many mm are missing there? The ruler along that edge shows it: 1 tick = 1 mm (Enter if you can't tell)"
		}
		gone, known := s.mm(q)
		if !known {
			gone = 2
		}
		d := int(math.Round(gone * s.dpm()))
		a := &s.printer.Adjust
		what := fmt.Sprintf("moved %.0f mm ", gone)
		switch side {
		case "t":
			a.YDots += d
			what += "down"
		case "b":
			a.YDots -= d
			what += "up"
		case "l":
			a.XDots += d
			what += "to the right"
		case "r":
			if head := s.model.HeadPins; a.PrintWidthDots == 0 && head > 0 {
				a.PrintWidthDots = head // the print width setting may be narrower than the label
				what = "printed across the whole print head"
			} else {
				a.XDots -= d
				what += "to the left"
			}
		}
		s.say("Printing it again, %s.\n", what)
		s.note("  fix: %s (%+v)", what, *a)
		if !again() {
			return
		}
	}
	if !s.ask("Is ▲ TOP at the top edge, as the label comes out of the printer?", true) {
		if s.ask("Is the whole label upside down?", true) {
			s.printer.UpsideDown = true
			if again() {
				s.ask("Is ▲ TOP at the top edge now?", true)
			}
		}
	}
	if s.ask("Is the text mirrored (does it read backwards, like in a mirror)?", false) {
		s.printer.Mirror = true
		if again() {
			s.ask("Does the text read normally now?", true)
		}
	}
	s.ask("Is the print dark enough to read easily?", true)
	s.ask("Is the checkerboard even (no white lines across it)?", true)
}

func (s *session) card2() {
	s.say("\n— Label 2/5: part label with QR code and barcode —\n")
	if !s.print("2/5", printing.PartLabel("2/5")) {
		return
	}
	s.ask("Are the QR code and the barcode sharp, with clean edges?", true)
	s.ask("Point your phone's camera at the QR code. Does it show a link (partdb.example.org)? You don't need to open it.", true)
	s.choose("If you have a barcode scanner: does it read the barcode? y = yes, n = no, Enter = no scanner:", "y", "n")
}

func (s *session) card3() {
	s.say("\n— Labels 3a/5 and 3b/5: two labels in one job —\n")
	src := func(m *driver.Media) ([]byte, error) {
		return pdfgen.TestCardsTagged(m.WidthMM, m.LengthMM, []string{"batch of two", s.printer.Model}, "3a/5", "3b/5"), nil
	}
	if !s.print("3a+3b/5", src) {
		return
	}
	s.ask("Did two labels come out, 3a/5 and 3b/5, each on its own label (nothing across the gap)?", true)
}

func (s *session) card4() {
	if s.square() {
		s.say("\n— Label 4/5 is skipped: it checks turning a label sideways, which can't show on square labels —\n")
		s.note("Label 4/5: skipped (square labels)")
		return
	}
	s.say("\n— Label 4/5: a label made the other way round (pdb-print turns it) —\n")
	src := func(m *driver.Media) ([]byte, error) {
		return pdfgen.TestCardTagged(m.LengthMM, m.WidthMM, []string{"turned", s.printer.Model}, "4/5"), nil
	}
	if !s.print("4/5", src) {
		return
	}
	s.ask("Is the whole frame of label 4/5 visible, and the text readable?", true)
}

func (s *session) card5() {
	s.say("\n— Step 5/5: printer status (no label) —\n")
	s.say("Open the printer's lid (print head) now, then press Enter.")
	s.line()
	got := s.status("head open")
	s.say("The printer reports: %s\n", got)
	s.say("Close the lid again (the printer may feed one blank label), then press Enter.")
	s.line()
	s.say("The printer reports: %s\n", s.status("head closed"))
}

// status reads and records the printer's status; returns it in words.
func (s *session) status(when string) string {
	if s.printer == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	st, err := s.o.Svc.Status(ctx, s.printer)
	text := ""
	switch {
	case err != nil:
		text = "error: " + err.Error()
	default:
		text = string(st.Code)
		if st.Detail != "" {
			text += " (" + st.Detail + ")"
		}
	}
	s.rep.Queries = append(s.rep.Queries, report.Query{Target: s.printer.Transport.Host, Name: "status " + when, Reply: text})
	s.note("Status %s: %s", when, text)
	return text
}

// restore sends the printer's own page settings back, since ours stay until it is switched off.
func (s *session) restore() {
	if !s.printed || s.printer == nil {
		return
	}
	pw, ll := s.setup["pw"], s.setup["ll"]
	if pw == 0 || ll == 0 {
		s.note("Settings not restored: the printer didn't report them. Switching it off and on resets them.")
		s.say("\nSwitch the printer off and on once to reset its settings.\n")
		return
	}
	cmd := s.restoreCmd()
	t, err := transport.New(s.printer.Transport)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
		defer cancel()
		if err = t.Open(ctx); err == nil {
			_, err = t.Write([]byte(cmd))
			t.Close()
		}
	}
	s.rep.Queries = append(s.rep.Queries, report.Query{Target: s.printer.Transport.Host, Name: "restore settings", Sent: cmd, Err: errText(err)})
	if err != nil {
		s.say("\nCouldn't put the printer's settings back (%v): switch it off and on once.\n", err)
		return
	}
	s.say("\nThe printer's own settings are back as they were.\n")
}

// restoreCmd is a format without fields that sets the printer's own page settings again.
func (s *session) restoreCmd() string {
	return fmt.Sprintf("^XA^PON^PW%d^LL%d^LH%d,%d^LS%d^LT%d^XZ\r\n",
		s.setup["pw"], s.setup["ll"], s.setup["lhx"], s.setup["lhy"], s.setup["ls"], s.setup["lt"])
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// write saves the summary and the masked report data next to the program.
func (s *session) write() (string, error) {
	model := "printer"
	if s.model != nil {
		model = s.model.ID
	}
	name := fmt.Sprintf("pdb-print-try-%s-%s.txt", model, s.o.Now().Format("20060102-1504"))
	path := filepath.Join(s.o.Dir, name)
	data, err := s.rep.JSON(false)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "pdb-print try – %s – %s\n\n", s.rep.Tool, s.o.Now().Format("2006-01-02 15:04"))
	for _, l := range s.sum {
		fmt.Fprintln(&b, s.rep.Mask(l)) // error texts can name addresses
	}
	fmt.Fprintf(&b, "\n--- data (addresses, serial numbers and names masked) ---\n%s\n", data)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	s.say("\nDone. The results are in %s\nSend this file and a photo of all the labels to Francis.\n", path)
	return path, nil
}
