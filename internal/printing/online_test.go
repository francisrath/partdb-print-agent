package printing

import (
	"context"
	"net"
	"strconv"
	"testing"

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
func TestOnlineConcurrentUsesLastState(t *testing.T) {
	s := service(t, nil)
	s.OnlineCheck = func(*config.Printer) bool { return false }
	p := tcpPrinter("ql", "127.0.0.1:9")
	if s.Online(p) {
		t.Fatal("offline printer online")
	}
	s.mu.Lock()
	s.online[onlineKey(p)] = onlineState{} // expired
	s.mu.Unlock()
	lock := s.printerLock(p.ID)
	lock.Lock()
	got := s.Online(p) // lock held by "the other heartbeat"
	lock.Unlock()
	if got {
		t.Error("printer reported online while another check held the lock")
	}
	moved := tcpPrinter("ql", "127.0.0.1:10")
	calls := 0
	s.OnlineCheck = func(*config.Printer) bool { calls++; return true }
	if !s.Online(moved) || calls != 1 {
		t.Error("cache ignored the changed address")
	}
}
