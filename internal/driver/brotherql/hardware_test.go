//go:build hardware

package brotherql

import (
	"context"
	"os"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/transport/tcp"
)

// Status only — never prints. PDB_PRINT_QL_ADDR=192.168.1.50:9100 go test -tags hardware ./internal/driver/brotherql
func TestHardwareStatus(t *testing.T) {
	addr := os.Getenv("PDB_PRINT_QL_ADDR")
	if addr == "" {
		t.Skip("PDB_PRINT_QL_ADDR not set")
	}
	tr := tcp.New(addr)
	if err := tr.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	s, err := New().Status(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v media=%+v", s, s.Media)
}
