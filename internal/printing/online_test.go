package printing

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

func tcpPrinter(id, addr string) *config.Printer {
	host, port, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(port)
	return &config.Printer{ID: id, Model: "QL-810W", Transport: config.Transport{Kind: "tcp", Host: host, Port: n}}
}

func TestOnlineCached(t *testing.T) {
	s := service(t, nil)
	calls := 0
	s.OnlineCheck = func(*config.Printer) bool { calls++; return true }
	p := tcpPrinter("ql", "127.0.0.1:9")
	first, second := s.Online(p), s.Online(p)
	if !first || !second || calls != 1 {
		t.Errorf("calls %d, want 1 within the cache window", calls)
	}
}

// A printer busy with a job is online, and the check must not open its device (usblp
// allows one open at a time: the job would fail with EBUSY).
func TestOnlineBusyPrinter(t *testing.T) {
	s := service(t, nil)
	s.OnlineCheck = func(*config.Printer) bool { t.Error("checked a printer that is printing"); return false }
	lock := s.printerLock("pt")
	lock.Lock()
	defer lock.Unlock()
	s.setPrinting("pt", true)
	defer s.setPrinting("pt", false)
	if !s.Online(ptPrinter("", "")) {
		t.Error("busy printer reported offline")
	}
}

func TestOnlineTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	s := service(t, nil)
	if !s.Online(tcpPrinter("up", ln.Addr().String())) {
		t.Error("listening printer offline")
	}
	addr := ln.Addr().String()
	ln.Close()
	if s.Online(tcpPrinter("down", addr)) {
		t.Error("closed port online")
	}
}

// A status check on a USB printer during a job must not open the device (usblp allows one
// open at a time): it answers BUSY, for QLs as for the PT.
func TestStatusUSBDuringJobIsBusy(t *testing.T) {
	s := service(t, nil)
	s.OpenTransport = func(*config.Printer) driver.Transport { t.Error("opened a USB printer that is printing"); return nil }
	p := &config.Printer{ID: "ql-usb", Model: "QL-810W", Transport: config.Transport{Kind: "usb", USBID: "04f9:209c"}}
	lock := s.printerLock(p.ID)
	lock.Lock()
	defer lock.Unlock()
	st, err := s.Status(context.Background(), p)
	if err != nil || st.Code != driver.Busy {
		t.Errorf("%+v %v", st, err)
	}
}

// Two heartbeats at once: the one that can't take the lock uses the last known state
// instead of claiming "printing"; the cache follows the printer's address.
// The cache follows the printer's address.
func TestOnlineCacheFollowsAddress(t *testing.T) {
	s := service(t, nil)
	s.OnlineCheck = func(*config.Printer) bool { return false }
	if s.Online(tcpPrinter("ql", "127.0.0.1:9")) {
		t.Fatal("offline printer online")
	}
	calls := 0
	s.OnlineCheck = func(*config.Printer) bool { calls++; return true }
	if !s.Online(tcpPrinter("ql", "127.0.0.1:10")) || calls != 1 {
		t.Error("cache ignored the changed address")
	}
}

// A printing job makes the printer online even if the last check said offline.
func TestOnlineJobBeatsStaleOffline(t *testing.T) {
	s := service(t, nil)
	s.OnlineCheck = func(*config.Printer) bool { return false }
	p := tcpPrinter("ql", "127.0.0.1:9")
	s.Online(p)
	s.mu.Lock()
	s.online[onlineKey(p)] = onlineState{} // expired "offline"
	s.mu.Unlock()
	lock := s.printerLock(p.ID)
	lock.Lock() // a job
	defer lock.Unlock()
	s.setPrinting(p.ID, true)
	defer s.setPrinting(p.ID, false)
	if !s.Online(p) {
		t.Error("printer printing a job reported offline")
	}
}

// Two heartbeats at once share one real check instead of guessing.
func TestOnlineConcurrentShareOneCheck(t *testing.T) {
	s := service(t, nil)
	var mu sync.Mutex
	calls := 0
	s.OnlineCheck = func(*config.Printer) bool {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		return false
	}
	p := tcpPrinter("ql", "127.0.0.1:9")
	res := make(chan bool, 2)
	for range 2 {
		go func() { res <- s.Online(p) }()
	}
	if a, b := <-res, <-res; a || b || calls != 1 {
		t.Errorf("results %v %v, %d checks; want both offline from one check", a, b, calls)
	}
}

// The printer lock held by something other than a job (a status check, a USB probe that
// outran its timeout) doesn't make an offline printer online.
func TestOnlineLockWithoutJobKeepsLastState(t *testing.T) {
	s := service(t, nil)
	s.OnlineCheck = func(*config.Printer) bool { return false }
	p := tcpPrinter("ql", "127.0.0.1:9")
	s.Online(p)
	s.mu.Lock()
	s.online[onlineKey(p)] = onlineState{} // expired "offline"
	s.mu.Unlock()
	lock := s.printerLock(p.ID)
	lock.Lock() // e.g. a pending USB probe, no job
	defer lock.Unlock()
	if s.Online(p) {
		t.Error("printer locked by a probe reported online")
	}
}

// Reversed publication: check A has returned its result but not stored it yet while check
// B arrives. Checks are serialised, so B waits (it never runs its own check) until A has
// stored its result, and then uses it: nothing can overwrite a newer result later.
func TestOnlineNoReversedPublication(t *testing.T) {
	s := service(t, nil)
	calls := 0
	s.OnlineCheck = func(*config.Printer) bool { calls++; return calls != 1 } // A: offline, B would be online
	atPublish, release := make(chan struct{}), make(chan struct{})
	s.beforePublish = func() { close(atPublish); <-release; s.beforePublish = nil } // A's result is in hand
	bWaiting := make(chan struct{})
	p := tcpPrinter("ql", "127.0.0.1:9")
	a := make(chan bool, 1)
	go func() { a <- s.Online(p) }()
	<-atPublish
	s.waitingForCheck = func() { close(bWaiting) }
	b := make(chan bool, 1)
	go func() { b <- s.Online(p) }()
	<-bWaiting // B is blocked behind A's check, deterministically
	close(release)
	if ra, rb := <-a, <-b; ra || rb || calls != 1 {
		t.Errorf("A %v, B %v, %d checks: B must take A's stored result", ra, rb, calls)
	}
	if up, fresh := s.cachedOnline(onlineKey(p)); up || !fresh {
		t.Errorf("cache %v fresh %v", up, fresh)
	}
}

// A job that starts while "offline" is cached still makes the printer online.
func TestOnlineJobBeatsFreshOffline(t *testing.T) {
	s := service(t, nil)
	s.OnlineCheck = func(*config.Printer) bool { return false }
	p := tcpPrinter("ql", "127.0.0.1:9")
	s.Online(p) // fresh "offline" in the cache
	s.setPrinting(p.ID, true)
	defer s.setPrinting(p.ID, false)
	if !s.Online(p) {
		t.Error("printing printer reported offline from the cache")
	}
}

// A USB open that outruns the probe timeout: Online answers offline on time, keeps the
// printer locked until the open really ends (no EBUSY for a starting job), and a call in
// the meantime keeps the last state instead of claiming "printing".
func TestOnlineSlowUSBProbe(t *testing.T) {
	s := service(t, nil)
	s.probeTimeout = 100 * time.Millisecond
	release := make(chan struct{})
	s.openDevice = func(string) (io.Closer, error) { <-release; return io.NopCloser(nil), nil }
	p := &config.Printer{ID: "pt", Model: "PT-9500PC", Transport: config.Transport{Kind: "usb", Device: "/dev/usb/lp9"}}
	start := time.Now()
	if s.Online(p) || time.Since(start) > time.Second {
		t.Fatalf("slow probe: online or late (%v)", time.Since(start))
	}
	lock := s.printerLock(p.ID)
	if lock.TryLock() {
		lock.Unlock()
		t.Fatal("printer unlocked while the probe's open is still running")
	}
	s.mu.Lock()
	s.online[onlineKey(p)] = onlineState{} // expired
	s.mu.Unlock()
	if s.Online(p) {
		t.Error("probe-held lock reported as printing")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for !lock.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("printer never unlocked after the probe ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	lock.Unlock()
}

// opens counts connections to a BLE printer.
type opens struct {
	silent
	n *int
}

func (o *opens) Open(context.Context) error { *o.n++; return nil }

// A Bluetooth printer (LE or Classic) takes one connection, so the online check never connects: the
// printer is online while a status, a job or a discovery scan found it working in the last 10 minutes.
func TestOnlineBluetooth(t *testing.T) {
	for _, kind := range []string{"ble", "bt"} {
		t.Run(kind, func(t *testing.T) {
			f := &fakeCat{}
			s, p := catService(t, f)
			p.Transport.Kind = kind
			n := 0
			s.OpenTransport = func(*config.Printer) driver.Transport { return &opens{silent{kind: kind}, &n} }
			online := func(want bool, when string) {
				t.Helper()
				before := n
				if got := s.Online(p); got != want {
					t.Errorf("%s: online %v, want %v", when, got, want)
				}
				if n != before {
					t.Errorf("%s: the online check connected", when)
				}
			}
			online(false, "never seen")
			if _, err := s.Status(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			online(true, "after a status")

			s.mu.Lock()
			s.seen[onlineKey(p)] = time.Now().Add(-11 * time.Minute)
			s.online = nil
			s.mu.Unlock()
			online(false, "seen 11 minutes ago")

			if _, err := s.Print(context.Background(), p, "", labels(1)); err != nil {
				t.Fatal(err)
			}
			online(true, "after a job")
			f.results = []error{errors.New("link dropped")}
			if _, err := s.Print(context.Background(), p, "", labels(1)); err == nil {
				t.Fatal("job didn't fail")
			}
			online(false, "after a job lost the printer")

			s.SeenBluetooth(p)
			online(true, "found in a scan")
		})
	}
}

// A sighting recorded while a check runs beats the check's older "offline".
func TestOnlineBLESightingDuringCheck(t *testing.T) {
	s, p := catService(t, &fakeCat{})
	s.beforePublish = func() { s.beforePublish = nil; s.SeenBluetooth(p) }
	if !s.Online(p) {
		t.Error("check reported offline over a newer sighting")
	}
	if !s.Online(p) {
		t.Error("stale offline cached over a newer sighting")
	}
}

// A job that fails on its document (or rendering) still reached the printer: the sighting stays.
func TestOnlineBLEDocumentErrorKeepsSighting(t *testing.T) {
	s, p := catService(t, &fakeCat{})
	s.SeenBluetooth(p)
	noDoc := func(*driver.Media) ([]byte, error) { return nil, errors.New("document unavailable") }
	if _, err := s.Print(context.Background(), p, "", noDoc); err == nil {
		t.Fatal("job didn't fail")
	}
	if _, err := s.Print(context.Background(), p, "", PDF([]byte("not a pdf"))); err == nil {
		t.Fatal("job didn't fail")
	}
	if !s.Online(p) {
		t.Error("a document error cleared the sighting")
	}
}

// A Classic printer that prints but ignores DLE EOT stays online: silence on a link that opened says
// nothing about its reach; only a link failure clears the sighting.
func TestOnlineBluetoothSilentStatusKeepsSighting(t *testing.T) {
	f := &fakeCat{statusErr: driver.ErrNoReadback}
	s, p := catService(t, f)
	p.Transport.Kind = "bt"
	s.OpenTransport = func(*config.Printer) driver.Transport { return &silent{kind: "bt"} }
	s.SeenBluetooth(p)
	if _, err := s.Status(context.Background(), p); !errors.Is(err, driver.ErrNoReadback) {
		t.Fatalf("err = %v", err)
	}
	if !s.Online(p) {
		t.Error("a silent status took the printer offline")
	}
}

// An edited RFCOMM channel is a new route: it needs its own sighting.
func TestOnlineKeyChannel(t *testing.T) {
	p := &config.Printer{ID: "rego", Transport: config.Transport{Kind: "bt", Address: "AA:BB:CC:DD:EE:FF", Channel: 1}}
	k := onlineKey(p)
	p.Transport.Channel = 2
	if onlineKey(p) == k {
		t.Error("the channel isn't part of the key")
	}
}
