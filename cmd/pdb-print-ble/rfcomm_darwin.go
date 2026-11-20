//go:build cgo

package main

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework IOBluetooth -framework Foundation
#include <stdlib.h>
#include "rfcomm_darwin.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/francisrath/partdb-print-agent/internal/blebridge"
	"github.com/francisrath/partdb-print-agent/internal/blecore"
	"github.com/francisrath/partdb-print-agent/internal/transport/bt"
)

// IOBluetooth delivers its callbacks on the main thread's run loop: the main goroutine stays on the main
// thread, which runs that loop (btRunMain) while another goroutine does the work.
func init() { runtime.LockOSThread() }

const (
	rfcommConnect = 20 * time.Second // page timeout, SDP query (≤10 s) and channel open
	rfcommClose   = 3 * time.Second
)

// rf is the one RFCOMM link (or inquiry) of this process, shared with the IOBluetooth callbacks.
var rf = struct {
	out     *frames
	who     string
	opened  chan openResult
	closed  chan struct{}
	inquiry chan C.int
	isOpen  atomic.Bool
	closing atomic.Bool
}{opened: make(chan openResult, 1), closed: make(chan struct{}, 1), inquiry: make(chan C.int, 1)}

type openResult struct{ code, status, mtu int }

//export goOpenDone
func goOpenDone(code, status, mtu C.int) {
	select {
	case rf.opened <- openResult{int(code), int(status), int(mtu)}:
	default:
	}
}

//export goData
func goData(p unsafe.Pointer, n C.int) {
	if err := rf.out.send(blebridge.TypeNotify, C.GoBytes(p, n)); err != nil {
		log.Printf("data: %v", err)
	}
}

//export goClosed
func goClosed() {
	if rf.isOpen.Load() && !rf.closing.Load() {
		// as for BLE: the client learns of the dropped link from this process ending
		fmt.Fprintf(os.Stderr, "%sprinter %s disconnected\n", fatalMark, rf.who)
		os.Exit(1)
	}
	select {
	case rf.closed <- struct{}{}:
	default:
	}
}

//export goInquiryDone
func goInquiryDone(status C.int) {
	select {
	case rf.inquiry <- status:
	default:
	}
}

// onRunLoop runs f on another goroutine while the main thread runs its run loop.
func onRunLoop(f func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- f()
		C.btStopMain()
	}()
	C.btRunMain()
	return <-done
}

var errBluetoothOff = fmt.Errorf("%w: Bluetooth is off", blecore.ErrNoBluetooth)

// classicDevice is one line of scan-classic's output (ble.ClassicDevice in pdb-print).
type classicDevice struct {
	Address string `json:"address"`
	Name    string `json:"name"`
	Class   uint32 `json:"class"`
}

// devices decodes and frees a device list from btPaired or btInquiryStop.
func devices(s *C.char) []classicDevice {
	defer C.free(unsafe.Pointer(s))
	var devs []classicDevice
	if err := json.Unmarshal([]byte(C.GoString(s)), &devs); err != nil {
		log.Printf("device list: %v", err)
	}
	for i := range devs { // IOBluetooth writes aa-bb-cc-dd-ee-ff
		devs[i].Address = strings.ToUpper(strings.ReplaceAll(devs[i].Address, "-", ":"))
	}
	return devs
}

// openRFCOMM connects to the printer's serial port. Incoming data goes to out as notifications.
func openRFCOMM(req blebridge.OpenRequest, out *frames) (link, error) {
	if C.btPowered() == 0 {
		return link{}, errBluetoothOff
	}
	addr := req.Address
	if addr == "" {
		var named []bt.Device
		for _, d := range devices(C.btPaired()) {
			named = append(named, bt.Device{Name: d.Name, Address: d.Address})
		}
		var err error
		if addr, err = bt.Resolve(named, req.Name); err != nil {
			return link{}, errors.New(strings.TrimPrefix(err.Error(), "bt: ")) // pdb-print adds it back
		}
	}
	who := addr
	if req.Name != "" {
		who = req.Name + " (" + addr + ")"
	}
	rf.out, rf.who = out, who
	ca := C.CString(strings.ReplaceAll(addr, ":", "-"))
	defer C.free(unsafe.Pointer(ca))
	C.btOpen(ca, C.int(req.Channel))
	var r openResult
	select {
	case r = <-rf.opened:
	case <-time.After(rfcommConnect):
		C.btClose()
		return link{}, fmt.Errorf("%s did not answer – is it switched on and paired with this computer? (nothing within %v)", who, rfcommConnect)
	}
	switch r.code {
	case C.BT_OK:
	case C.BT_NO_SPP:
		C.btClose()
		return link{}, fmt.Errorf("%s has no serial port (SPP) service – is it the printer, and is it switched on?", who)
	case C.BT_BAD_ADDRESS:
		return link{}, fmt.Errorf("address %q isn't a Bluetooth address", req.Address)
	default:
		C.btClose()
		return link{}, fmt.Errorf("%s did not answer – is it switched on and paired with this computer? (IOReturn %#x)", who, uint32(r.status))
	}
	rf.isOpen.Store(true)
	log.Printf("connected to %s, %d bytes per write", who, r.mtu)
	return link{mtu: r.mtu, send: rfcommWrite, close: closeRFCOMM}, nil
}

func rfcommWrite(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if r := C.btWrite(unsafe.Pointer(&p[0]), C.int(len(p))); r != 0 {
		return fmt.Errorf("write: IOReturn %#x", uint32(r))
	}
	return nil
}

// closeRFCOMM closes the channel and waits for IOBluetooth to confirm.
func closeRFCOMM() {
	rf.closing.Store(true)
	C.btClose()
	select {
	case <-rf.closed:
	case <-time.After(rfcommClose):
		log.Printf("close not confirmed within %v", rfcommClose)
	}
}

func scanClassicCmd(args []string) error {
	fs := flag.NewFlagSet("scan-classic", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 8*time.Second, "how long to look for devices")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var devs []classicDevice
	if err := onRunLoop(func() (err error) {
		devs, err = scanClassic(ctx, *timeout)
		return err
	}); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, d := range devs {
		if err := enc.Encode(d); err != nil {
			return err
		}
	}
	return nil
}

// scanClassic lists the paired devices, then those an inquiry finds (once per address). Interrupted, it
// returns what it has.
func scanClassic(ctx context.Context, timeout time.Duration) ([]classicDevice, error) {
	if C.btPowered() == 0 {
		return nil, errBluetoothOff
	}
	devs := devices(C.btPaired())
	C.btInquiryStart(C.int(min(max(timeout/time.Second, 1), 60)))
	select { // the inquiry would run past its length asking new devices for their names: stop it on time
	case st := <-rf.inquiry:
		if st != 0 {
			log.Printf("inquiry: IOReturn %#x", uint32(st))
		}
	case <-ctx.Done():
	case <-time.After(timeout):
	}
	seen := map[string]bool{}
	var all []classicDevice
	for _, d := range append(devs, devices(C.btInquiryStop())...) {
		if !seen[d.Address] {
			seen[d.Address] = true
			all = append(all, d)
		}
	}
	return all, nil
}
