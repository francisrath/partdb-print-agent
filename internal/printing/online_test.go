package printing

import (
	"net"
	"strconv"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/config"
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
