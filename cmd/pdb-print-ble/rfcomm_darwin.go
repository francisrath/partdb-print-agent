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

	"github.com/francisrath/pdb-print/internal/blebridge"
	"github.com/francisrath/pdb-print/internal/blecore"
	"github.com/francisrath/pdb-print/internal/transport/bt"
)

// IOBluetooth delivers its callbacks on the main thread's run loop: the main goroutine stays on the main
// thread, which runs that loop (btRunMain) while another goroutine does the work.
func init() { runtime.LockOSThread() }

const (
	rfcommConnect  = 20 * time.Second // page timeout, SDP query (≤10 s) and channel open, retry included
	rfcommAttempt  = 8 * time.Second  // for the channel to open before one retry
	rfcommRetryMin = 5 * time.Second  // left before the connect bound, else no retry
	// A helper that exits before macOS has dropped the link leaves the printer unreachable for the next
	// job (as with BLE): wait for the channel, then the connection, within the client's close grace.
	channelCloseWait = blecore.NotifyOffWait
	disconnectWait   = blecore.DisconnectWait
	staleLinkWait    = 2 * time.Second // for a link left by the previous helper to drop before Open
)

// rf is the one RFCOMM link (or inquiry) of this process, shared with the IOBluetooth callbacks.
var rf = struct {
	out     *blebridge.Frames
	who     string
	opening chan struct{}
	opened  chan openResult
	closed  chan struct{}
	inquiry chan C.int
	isOpen  atomic.Bool
	closing atomic.Bool
}{opening: make(chan struct{}, 1), opened: make(chan openResult, 1), closed: make(chan struct{}, 1), inquiry: make(chan C.int, 1)}

type openResult struct{ code, status, mtu int }

//export goOpenDone
func goOpenDone(code, status, mtu C.int) {
	select {
	case rf.opened <- openResult{int(code), int(status), int(mtu)}:
	default:
	}
}

//export goOpening
func goOpening() {
	select {
	case rf.opening <- struct{}{}:
	default:
	}
}

// goData runs on the main thread; writing stdout there can't stall it for long, since the client's
// readLoop always reads.
//
//export goData
func goData(p unsafe.Pointer, n C.int) {
	if err := rf.out.Send(blebridge.TypeNotify, C.GoBytes(p, n)); err != nil {
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

// devices decodes and frees a device list from btPaired or btInquiryStop.
func devices(s *C.char) []blebridge.ClassicDevice {
	defer C.free(unsafe.Pointer(s))
	var devs []blebridge.ClassicDevice
	if err := json.Unmarshal([]byte(C.GoString(s)), &devs); err != nil {
		log.Printf("device list: %v", err)
	}
	for i := range devs { // IOBluetooth writes aa-bb-cc-dd-ee-ff
		devs[i].Address = strings.ToUpper(strings.ReplaceAll(devs[i].Address, "-", ":"))
	}
	return devs
}

// openRFCOMM connects to the printer's serial port. Incoming data goes to out as notifications.
func openRFCOMM(req blebridge.OpenRequest, out *blebridge.Frames) (blebridge.Link, error) {
	if C.btPowered() == 0 {
		return blebridge.Link{}, errBluetoothOff
	}
	addr := req.Address
	if addr == "" {
		var named []bt.Device
		for _, d := range devices(C.btPaired()) {
			named = append(named, bt.Device{Name: d.Name, Address: d.Address})
		}
		var err error
		if addr, err = bt.Resolve(named, req.Name); err != nil {
			return blebridge.Link{}, errors.New(strings.TrimPrefix(err.Error(), "bt: ")) // pdb-print adds it back
		}
	}
	who := addr
	if req.Name != "" {
		who = req.Name + " (" + addr + ")"
	}
	rf.out, rf.who = out, who
	ca := C.CString(strings.ReplaceAll(addr, ":", "-"))
	defer C.free(unsafe.Pointer(ca))
	if waitFor(staleLinkWait, func() bool { return C.btIsConnected(ca) == 0 }) {
		log.Printf("%s is still connected (an earlier job?), opening anyway", who)
	}
	C.btOpen(ca, C.int(req.Channel))
	var r openResult
	end := time.Now().Add(rfcommConnect)
	deadline := time.After(rfcommConnect)
	var attempt <-chan time.Time // runs while the channel opens
	retried := false
wait:
	for {
		select {
		case <-rf.opening:
			attempt = time.After(rfcommAttempt)
		case r = <-rf.opened:
			break wait
		case <-attempt: // the retry's attempt is bounded by deadline only
			attempt = nil
			// a retry needs a few seconds of its own (SDP's channel-1 fallback opens late)
			if !retried && time.Until(end) >= rfcommRetryMin {
				retried = true
				log.Printf("the channel to %s didn't open within %v, trying once more", who, rfcommAttempt)
				C.btRetryOpen()
			}
		case <-deadline:
			dropLink()
			return blebridge.Link{}, fmt.Errorf("%s did not answer – is it switched on and paired with this computer? (nothing within %v)", who, rfcommConnect)
		}
	}
	switch r.code {
	case C.BT_OK:
	case C.BT_NO_SPP:
		dropLink()
		return blebridge.Link{}, fmt.Errorf("%s has no serial port (SPP) service – is it the printer, and is it switched on?", who)
	case C.BT_BAD_ADDRESS:
		return blebridge.Link{}, fmt.Errorf("address %q isn't a Bluetooth address", addr)
	default:
		dropLink()
		return blebridge.Link{}, fmt.Errorf("%s did not answer – is it switched on and paired with this computer? (IOReturn %#x)", who, uint32(r.status))
	}
	rf.isOpen.Store(true)
	log.Printf("connected to %s, %d bytes per write", who, r.mtu)
	return blebridge.Link{MTU: r.mtu, Send: rfcommWrite, Close: closeRFCOMM}, nil
}

func rfcommWrite(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if r := C.btWrite(unsafe.Pointer(&p[0]), C.int(len(p))); r != 0 {
		// macOS gives up on a printer that stopped taking data (seen: 0xe00002e7 after about a minute
		// with the cover open)
		return fmt.Errorf("the printer stopped taking data – cover open, out of paper or switched off? (IOReturn %#x)", uint32(r))
	}
	return nil
}

// closeRFCOMM closes the channel, then the connection, and waits for IOBluetooth to confirm each.
func closeRFCOMM() {
	rf.closing.Store(true)
	C.btClose()
	select {
	case <-rf.closed:
	case <-time.After(channelCloseWait):
		log.Printf("channel close not confirmed within %v", channelCloseWait)
	}
	C.btCloseConnection()
	if waitFor(disconnectWait, func() bool { return C.btIsConnected(nil) == 0 }) {
		log.Printf("disconnect not confirmed within %v", disconnectWait)
	}
}

// dropLink ends a failed open: the channel if any, then the connection (the SDP query may have opened it).
func dropLink() {
	C.btClose()
	C.btCloseConnection()
	waitFor(disconnectWait, func() bool { return C.btIsConnected(nil) == 0 })
}

// waitFor polls done every 100 ms until it holds or d has passed; it reports whether d passed.
func waitFor(d time.Duration, done func() bool) (timedOut bool) {
	for end := time.Now().Add(d); !done(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(end) {
			return true
		}
	}
	return false
}

func scanClassicCmd(args []string) error {
	fs := flag.NewFlagSet("scan-classic", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 8*time.Second, "how long to look for devices")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var devs []blebridge.ClassicDevice
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

// scanClassic lists the paired devices, then those an inquiry finds (once per address, the inquiry filling in
// a paired device's missing name or class). Interrupted, it returns what it has.
func scanClassic(ctx context.Context, timeout time.Duration) ([]blebridge.ClassicDevice, error) {
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
	return blebridge.MergeClassic(devs, devices(C.btInquiryStop())), nil
}
