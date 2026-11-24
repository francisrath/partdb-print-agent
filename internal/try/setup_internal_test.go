package try

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/francisrath/pdb-print/internal/report"
)

// The settings put back at the end come from the printer's real ^HZA and ^HH replies.
func TestReadSetupRealReplies(t *testing.T) {
	t.Parallel()
	hza, err := os.ReadFile("../driver/zpl/zpltest/testdata/hza")
	if err != nil {
		t.Fatal(err)
	}
	hh, _ := os.ReadFile("../driver/zpl/zpltest/testdata/hh")
	z := string(hza)
	z = strings.Replace(z, "<LABEL-SHIFT MIN='-9999' MAX='9999'>0<", "<LABEL-SHIFT MIN='-9999' MAX='9999'>5<", 1)
	z = strings.Replace(z, "<X-AXIS MIN='0' MAX='32000'>0<", "<X-AXIS MIN='0' MAX='32000'>3<", 1)
	z = strings.Replace(z, "<Y-AXIS MIN='0' MAX='32000'>0<", "<Y-AXIS MIN='0' MAX='32000'>4<", 1)
	z = strings.Replace(z, "<LABEL-TOP MIN='-120' MAX='80'>\r\n<CURRENT>0<", "<LABEL-TOP MIN='-120' MAX='80'>\r\n<CURRENT>-12<", 1)
	s := &session{setup: map[string]int{}, rep: &report.Report{Queries: []report.Query{
		{Name: "zpl ^HH", Reply: report.Escape(hh)}, {Name: "zpl ^HZA", Reply: report.Escape([]byte(z))}}}}
	s.readSetup()
	want := map[string]int{"pw": 799, "ll": 812, "ls": 5, "lhx": 3, "lhy": 4, "lt": -12}
	for k, v := range want {
		if got, ok := s.setup[k]; !ok || got != v {
			t.Errorf("%s = %d (found %v), want %d", k, got, ok, v)
		}
	}
	if cmd := s.restoreCmd(); cmd != "^XA^PON^PW799^LL812^LH3,4^LS5^LT-12^XZ\r\n" {
		t.Errorf("restore %q", cmd)
	}
}

// Settings the printer didn't report aren't "restored" as zeros.
func TestRestoreOnlyReported(t *testing.T) {
	t.Parallel()
	s := &session{setup: map[string]int{"pw": 799, "ll": 812}}
	if cmd := s.restoreCmd(); cmd != "^XA^PON^PW799^LL812^XZ\r\n" {
		t.Errorf("restore %q", cmd)
	}
}

// Two sessions in the same minute keep both reports.
func TestReportNamesUnique(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := func() time.Time { return time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC) }
	var paths []string
	for range 2 {
		s := &session{o: Options{Dir: dir, Now: now, Out: io.Discard}, rep: &report.Report{}, setup: map[string]int{}}
		p, err := s.write()
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	if paths[0] == paths[1] {
		t.Errorf("same file twice: %s", paths[0])
	}
}

// The closing message names only the settings that were put back.
func TestRestoredNames(t *testing.T) {
	t.Parallel()
	s := &session{setup: map[string]int{"pw": 799, "ll": 812}}
	if got := s.restoredNames(); got != "print width and label length" {
		t.Errorf("%q", got)
	}
	s.setup["lhx"], s.setup["lhy"], s.setup["ls"], s.setup["lt"] = 0, 0, 0, 0
	if got := s.restoredNames(); got != "print width, label length, label home, label shift and label top" {
		t.Errorf("%q", got)
	}
}
