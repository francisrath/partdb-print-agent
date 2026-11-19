package try

import (
	"os"
	"strings"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/report"
)

// The settings put back at the end come from the printer's real ^HZA and ^HH replies.
func TestReadSetupRealReplies(t *testing.T) {
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
