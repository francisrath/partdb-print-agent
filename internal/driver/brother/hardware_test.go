//go:build hardware

package brother

import (
	"context"
	"os"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/transport/tcp"
	"github.com/francisrath/partdb-print-agent/internal/transport/usb"
)

// Status only — never prints.
//
//	PDB_PRINT_QL_ADDR=192.168.1.50:9100 go test -tags hardware ./internal/driver/brother
//	PDB_PRINT_PT_DEVICE=/dev/usb/lp0 go test -tags hardware ./internal/driver/brother
func TestHardwareStatus(t *testing.T) {
	for _, tc := range []struct {
		env, model string
		open       func(string) driver.Transport
	}{
		{"PDB_PRINT_QL_ADDR", "QL-810W", func(a string) driver.Transport { return tcp.New(a) }},
		{"PDB_PRINT_PT_DEVICE", "PT-9500PC", func(d string) driver.Transport { return usb.New(d) }},
	} {
		where := os.Getenv(tc.env)
		if where == "" {
			t.Logf("%s not set, skipping %s", tc.env, tc.model)
			continue
		}
		m, _ := devicedb.Default().Model(tc.model)
		d, err := driver.New(m, driver.Options{NoSNMP: true})
		if err != nil {
			t.Fatal(err)
		}
		tr := tc.open(where)
		if err := tr.Open(context.Background()); err != nil {
			t.Fatal(err)
		}
		s, err := d.Status(context.Background(), tr)
		_ = tr.Close()
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		t.Logf("%s: %+v media=%+v", tc.model, s, s.Media)
	}
}
