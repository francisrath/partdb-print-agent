//go:build darwin && cgo

// Command pdb-print-usb is pdb-print's USB printer helper on macOS, where no kernel driver offers a
// printer device: it talks to the printer-class interface through IOUSBHost (cgo), so pdb-print itself
// stays cgo-free. It is separate from pdb-print-ble, whose Bluetooth library asks for Bluetooth permission
// whenever it starts. Protocol on stdin/stdout: internal/blebridge (Kind "usb").
//
//	pdb-print-usb              bridge mode: frames on stdin/stdout, logs on stderr
//	pdb-print-usb list [--ids] one JSON object per USB printer; --ids opens each for its IEEE 1284 ID
package main

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework IOUSBHost -framework IOKit -framework Foundation
#include <stdlib.h>
#include "usb_darwin.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/francisrath/pdb-print/internal/blebridge"
)

// fatalMark starts the last stderr line when the helper exits on an error (internal/transport/ble reads it).
const fatalMark = "error: "

const (
	busyWait = 15 * time.Second // macOS's own print queue holds the interface up to ~10 s after its job
	// after that the printer is still finishing the queue's label: a QL-810W answered status 3.2 s after the
	// interface came free (2026-11-26). ponytail: a fixed wait; the helper doesn't speak printer languages.
	settle    = 5 * time.Second
	readSlice = 500 * time.Millisecond
	readSize  = 512
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("pdb-print-usb: ")
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "list":
		fs := flag.NewFlagSet("list", flag.ContinueOnError)
		ids := fs.Bool("ids", false, "read each printer's IEEE 1284 ID (opens it)")
		fs.Duration("timeout", 0, "ignored (pdb-print's helper client passes it)")
		if err = fs.Parse(os.Args[2:]); err == nil {
			err = list(*ids)
		}
	case len(os.Args) > 1:
		fmt.Fprintln(os.Stderr, "usage: pdb-print-usb [list [--ids]]")
		os.Exit(2)
	default:
		err = serve()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, fatalMark+err.Error())
		os.Exit(1)
	}
}

func list(ids bool) error {
	c := C.usbList(boolInt(ids))
	defer C.free(unsafe.Pointer(c))
	enc := json.NewEncoder(os.Stdout)
	for _, line := range strings.Split(strings.TrimRight(C.GoString(c), "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			continue
		}
		vid, _ := strconv.Atoi(f[1])
		pid, _ := strconv.Atoi(f[2])
		p := blebridge.USBPrinter{Device: "iokit:" + f[0], USBID: fmt.Sprintf("%04x:%04x", vid, pid), Serial: f[3], IEEE1284: f[4]}
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	return nil
}

func boolInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

func cErr(e *C.char) error {
	defer C.free(unsafe.Pointer(e))
	return errors.New(C.GoString(e))
}

func serve() error {
	out := &blebridge.Frames{W: os.Stdout}
	req, err := blebridge.ReadOpen(os.Stdin)
	if req == nil || err != nil {
		return err
	}
	if req.Kind != blebridge.KindUSB {
		err = fmt.Errorf("unknown link kind %q", req.Kind)
		_ = out.Send(blebridge.TypeError, []byte(err.Error()))
		return err
	}
	return blebridge.Relay(os.Stdin, out, func() (blebridge.Link, error) { return open(req.Device, out) })
}

// open opens the interface, waiting up to busyWait while another program has it, and forwards everything
// the printer sends as Notify frames until the link closes.
func open(device string, out *blebridge.Frames) (blebridge.Link, error) {
	id, err := strconv.ParseUint(strings.TrimPrefix(device, "iokit:"), 0, 64)
	if err != nil || !strings.HasPrefix(device, "iokit:") {
		return blebridge.Link{}, fmt.Errorf("device %q isn't a macOS USB printer (see pdb-print discover)", device)
	}
	var h unsafe.Pointer
	waited := false
	for start := time.Now(); ; {
		var e *C.char
		if h = C.usbOpen(C.uint64_t(id), &e); h != nil {
			if waited {
				log.Printf("waited for another program; holding the printer %v while it finishes", settle)
				time.Sleep(settle)
			}
			break
		}
		waited = true
		err := cErr(e)
		if strings.Contains(err.Error(), "not connected") || strings.Contains(err.Error(), "no raster printer interface") {
			return blebridge.Link{}, err
		}
		if time.Since(start) > busyWait {
			return blebridge.Link{}, fmt.Errorf("the printer is in use by another program (macOS's print queue?): %w", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	var stop sync.WaitGroup
	var closing sync.Once
	done := make(chan struct{})
	canRead := C.usbCanRead(h) != 0
	stop.Add(1)
	go func() {
		if !canRead { // a unidirectional printer never answers
			stop.Done()
			return
		}
		defer stop.Done()
		buf := C.malloc(readSize)
		defer C.free(buf)
		for {
			select {
			case <-done:
				return
			default:
			}
			var e *C.char
			n := C.usbRead(h, buf, readSize, C.int(readSlice.Milliseconds()), &e)
			if n < 0 { // the printer is gone or broken: end the link, so the client sees it (EOF, then this line)
				fmt.Fprintln(os.Stderr, fatalMark+cErr(e).Error())
				os.Exit(1)
			}
			if n > 0 {
				if err := out.Send(blebridge.TypeNotify, C.GoBytes(buf, n)); err != nil {
					return
				}
			}
		}
	}()
	send := func(p []byte) error {
		if len(p) == 0 {
			return nil
		}
		var e *C.char
		if C.usbWrite(h, unsafe.Pointer(&p[0]), C.int(len(p)), &e) != 0 {
			return cErr(e)
		}
		return nil
	}
	return blebridge.Link{MTU: blebridge.MaxFrame, NoReadback: !canRead, Send: send, Close: func() {
		closing.Do(func() {
			close(done)
			stop.Wait() // the read in flight ends within readSlice
			C.usbClose(h)
		})
	}}, nil
}
