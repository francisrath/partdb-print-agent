//go:build linux || windows

package bt

import (
	"context"

	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
	"github.com/francisrath/pdb-print/internal/transport"
)

// dialFunc connects to the printer, resolving name to an address when address is empty.
type dialFunc func(ctx context.Context, address, name string, channel int) (conn, error)

// register makes kind bt available with the OS's dial.
func register(dial dialFunc) {
	transport.Register("bt", func(c config.Transport, _ *devicedb.Model) (driver.Transport, error) {
		t := &Transport{Address: c.Address, Name: c.Name, Channel: c.Channel}
		t.dial = func(ctx context.Context) (conn, error) { return dial(ctx, t.Address, t.Name, t.Channel) }
		return t, nil
	})
}

// label names the printer in errors.
func label(address, name string) string {
	if name == "" {
		return address
	}
	return name + " (" + address + ")"
}
