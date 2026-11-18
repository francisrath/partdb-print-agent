//go:build hardware

package brotherpt

import (
	"context"
	"os"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/transport/usb"
)

// Status only — never prints. PDB_PRINT_PT_DEVICE=/dev/usb/lp0 go test -tags hardware ./internal/driver/brotherpt
func TestHardwareStatus(t *testing.T) {
	dev := os.Getenv("PDB_PRINT_PT_DEVICE")
	if dev == "" {
		t.Skip("PDB_PRINT_PT_DEVICE not set")
	}
	tr := usb.New(dev)
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
