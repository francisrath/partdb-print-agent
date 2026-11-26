package blebridge

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
)

// The helper side of the protocol, shared by pdb-print-ble and pdb-print-usb.

// Frames writes frames to the client from the request loop and the printer's data callback.
type Frames struct {
	mu sync.Mutex
	W  io.Writer
}

func (f *Frames) Send(typ byte, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Write(f.W, typ, p)
}

// Link is an open connection to a printer.
type Link struct {
	MTU        int
	NoReadback bool // the printer can't answer (USB: no bulk IN endpoint)
	Send       func([]byte) error
	Close      func()
}

// ReadOpen reads the client's Open frame; nil, nil when the input ends first.
func ReadOpen(in io.Reader) (*OpenRequest, error) {
	typ, p, err := Read(in)
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if typ != TypeOpen {
		return nil, fmt.Errorf("expected an open frame, got %q", typ)
	}
	var req OpenRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, fmt.Errorf("open request: %w", err)
	}
	return &req, nil
}

// Relay opens the link, then passes Writes to it until Close or end of input.
func Relay(in io.Reader, out *Frames, open func() (Link, error)) error {
	c, err := open()
	if err != nil {
		_ = out.Send(TypeError, []byte(err.Error()))
		return err
	}
	defer c.Close()
	ok, _ := json.Marshal(OK{MTU: c.MTU, NoReadback: c.NoReadback})
	if err := out.Send(TypeOK, ok); err != nil {
		return err
	}
	for {
		typ, p, err := Read(in)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch typ {
		case TypeClose:
			return nil
		case TypeWrite:
			answer, msg := TypeOK, []byte(nil)
			if err := c.Send(p); err != nil {
				log.Printf("write: %v", err)
				answer, msg = TypeError, []byte(err.Error())
			}
			if err := out.Send(answer, msg); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected frame %q", typ)
		}
	}
}
