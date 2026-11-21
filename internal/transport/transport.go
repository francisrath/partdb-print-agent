// Package transport is the registry of printer connections by config kind (tcp, usb, …).
// Each kind's package registers itself in init.
package transport

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
)

var (
	mu    sync.Mutex
	kinds = map[string]func(config.Transport, *devicedb.Model) (driver.Transport, error){}
)

// Register makes a transport kind available.
func Register(kind string, open func(config.Transport, *devicedb.Model) (driver.Transport, error)) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := kinds[kind]; dup {
		panic("transport: kind registered twice: " + kind)
	}
	kinds[kind] = open
}

// New returns an unopened transport for a printer's config. The model may be nil.
func New(c config.Transport, m *devicedb.Model) (driver.Transport, error) {
	mu.Lock()
	open, ok := kinds[c.Kind]
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k)
	}
	mu.Unlock()
	if !ok {
		sort.Strings(names)
		return nil, fmt.Errorf("unknown transport kind %q (known: %s)", c.Kind, strings.Join(names, ", "))
	}
	return open(c, m)
}
