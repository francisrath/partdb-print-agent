package driver

import (
	"context"
	"io"
	"slices"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
)

type fakeDriver struct {
	model *devicedb.Model
	opts  Options
}

func (fakeDriver) Status(context.Context, Transport) (*Status, error)      { return nil, nil }
func (fakeDriver) Print(context.Context, Transport, *Job) (*Result, error) { return nil, nil }
func (fakeDriver) Encode(io.Writer, *Job) error                            { return nil }

func TestRegistry(t *testing.T) {
	Register("zz-fake", func(m *devicedb.Model, o Options) (Driver, error) { return fakeDriver{m, o}, nil })
	m := &devicedb.Model{ID: "Fake-1", Driver: "zz-fake"}
	d, err := New(m, Options{Host: "h", SNMPCommunity: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if f := d.(fakeDriver); f.model != m || f.opts.Host != "h" || f.opts.SNMPCommunity != "c" {
		t.Errorf("model/options not passed through: %+v", f)
	}
	if _, err := New(&devicedb.Model{ID: "X", Driver: "nope"}, Options{}); err == nil {
		t.Error("unknown family accepted")
	} else if e, ok := err.(*Error); !ok || e.Code != Unsupported {
		t.Errorf("err = %v, want UNSUPPORTED", err)
	}
	if fs := Families(); !slices.Contains(fs, "zz-fake") || !slices.IsSorted(fs) {
		t.Errorf("families %v", fs)
	}
	defer func() {
		if recover() == nil {
			t.Error("registering a family twice didn't panic")
		}
	}()
	Register("zz-fake", func(*devicedb.Model, Options) (Driver, error) { return nil, nil })
}
