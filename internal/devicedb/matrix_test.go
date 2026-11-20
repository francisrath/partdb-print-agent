package devicedb_test

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
)

var update = flag.Bool("update", false, "rewrite the generated block of docs/printers.md")

// TestPrintersDoc keeps the support matrix in docs/printers.md in step with the device database.
func TestPrintersDoc(t *testing.T) {
	const path, begin, end = "../../docs/printers.md", "<!-- printers:begin -->\n", "<!-- printers:end -->"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i, j := strings.Index(doc, begin), strings.Index(doc, end)
	if i < 0 || j < i {
		t.Fatalf("%s: no %q … %q block", path, strings.TrimSpace(begin), end)
	}
	want := "\n" + devicedb.Default().MarkdownMatrix() + "\n"
	if got := doc[i+len(begin) : j]; got == want {
		return
	}
	if *update {
		if err := os.WriteFile(path, []byte(doc[:i+len(begin)]+want+doc[j:]), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Errorf("%s is out of date with the device database: run go test ./internal/devicedb -run TestPrintersDoc -update", path)
}
