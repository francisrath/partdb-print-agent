package printing

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/transport/usb"
)

// onlineFor is how long an online check counts: heartbeats can come every few seconds
// and from several Part-DB instances; printers aren't poked more often than this.
const onlineFor = 30 * time.Second

type onlineState struct {
	at time.Time
	ok bool
}

// Online reports whether the printer is reachable: a printer busy with a job is; a USB
// printer whose device is present and opens is; a network printer that accepts a connection
// on its print port within 2 s is (nothing is sent); a Bluetooth printer (LE or Classic) is while it
// was found working in the last 10 minutes (see SeenBluetooth). Results are cached for 30 s; checks of
// one printer run one at a time and share their result.
func (s *Service) Online(p *config.Printer) bool {
	if s.isPrinting(p.ID) {
		return true // a job started since the last check
	}
	key := onlineKey(p)
	if up, ok := s.cachedOnline(key); ok {
		return up
	}
	probe := s.lockFor(&s.probes, p.ID)
	if !probe.TryLock() { // a check is already running for this printer: wait for its result
		if s.waitingForCheck != nil {
			s.waitingForCheck()
		}
		probe.Lock()
	}
	defer probe.Unlock()
	if up, ok := s.cachedOnline(key); ok {
		return up
	}
	lock := s.printerLock(p.ID)
	if !lock.TryLock() {
		if s.isPrinting(p.ID) {
			return true // printing a job right now
		}
		// a status check or a USB probe that outran its timeout holds it: keep the last state
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.online[key].ok
	}
	start := time.Now()
	up, done := s.check(p) // under the printer lock: a USB open can't collide with a starting job
	if s.beforePublish != nil {
		s.beforePublish()
	}
	s.mu.Lock()
	if s.online == nil {
		s.online = map[string]onlineState{}
	}
	if at, ok := s.seen[key]; ok && !at.Before(start) { // not After: Windows' clock can give both the same instant
		up = true // SeenBluetooth during the check: newer than what the check found
	}
	s.online[key] = onlineState{time.Now(), up} // published before the lock is released
	s.mu.Unlock()
	select {
	case <-done:
		lock.Unlock()
	default: // a USB open/close outran its 2 s: keep jobs out until it really ends
		go func() { <-done; lock.Unlock() }()
	}
	return up
}

func (s *Service) setPrinting(id string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.printing == nil {
		s.printing = map[string]bool{}
	}
	s.printing[id] = on
}

func (s *Service) isPrinting(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.printing[id]
}

func (s *Service) cachedOnline(key string) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.online[key]
	return st.ok, ok && time.Since(st.at) < onlineFor
}

// check runs the reachability check; done is closed when it has fully finished.
func (s *Service) check(p *config.Printer) (bool, <-chan struct{}) {
	if s.OnlineCheck != nil {
		done := make(chan struct{})
		close(done)
		return s.OnlineCheck(p), done
	}
	return s.reachable(p)
}

// onlineKey keys the cache by printer and route (address, RFCOMM channel, ...), so an edited route is checked afresh.
func onlineKey(p *config.Printer) string {
	t := p.Transport
	return fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s|%s|%s|%d", p.ID, t.Kind, t.Host, t.Port, t.Device, t.USBID, t.Serial, t.Address, t.Name, t.Channel)
}

// bluetoothSeenFor is how long a Bluetooth printer counts as online after it was last found working. Checking
// would take its only connection (and drop a phone's or a job's), so it is never connected just to ask.
const bluetoothSeenFor = 10 * time.Minute

// SeenBluetooth records that a Bluetooth printer was found working (a status, a job, a BLE discovery scan).
func (s *Service) SeenBluetooth(p *config.Printer) {
	key := onlineKey(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	s.seen[key] = time.Now()
	delete(s.online, key) // the next check sees it at once
}

// noteBluetooth records how a status or job on a Bluetooth printer went: an answer means it's there; failing to
// reach it (a link failure) means it's gone. Other errors (no paper, a bad PDF) say nothing about its reach, nor
// does ErrNoReadback: a printer that prints but ignores status requests stays as seen.
func (s *Service) noteBluetooth(p *config.Printer, err error) {
	if !config.Bluetooth(p.Transport.Kind) {
		return
	}
	var de *driver.Error
	switch {
	case err == nil:
		s.SeenBluetooth(p)
	case errors.Is(err, driver.ErrNoReadback):
	case errors.As(commsError(err), &de) && de.Code == driver.CommsError:
		key := onlineKey(p)
		s.mu.Lock()
		delete(s.seen, key)
		delete(s.online, key)
		s.mu.Unlock()
	}
}

func (s *Service) reachable(p *config.Printer) (bool, <-chan struct{}) {
	done := make(chan struct{})
	t := p.Transport
	switch t.Kind {
	case "usb":
		dev := t.Device
		if t.USBID != "" {
			var err error
			if dev, err = usb.Find("", t.USBID, t.Serial); err != nil {
				close(done)
				return false, done
			}
		}
		open, timeout := s.openDevice, s.probeTimeout
		if open == nil {
			open = func(path string) (io.Closer, error) { return os.OpenFile(path, os.O_RDWR, 0) }
		}
		if timeout == 0 {
			timeout = 2 * time.Second
		}
		opened := make(chan bool, 1)
		go func() { // usblp opens don't block, but a check must never hang a heartbeat
			defer close(done)
			f, err := open(dev)
			opened <- err == nil && f.Close() == nil
		}()
		select {
		case ok := <-opened:
			return ok, done
		case <-time.After(timeout):
			return false, done
		}
	case "ble", "bt": // config.Bluetooth
		close(done)
		s.mu.Lock()
		defer s.mu.Unlock()
		at, ok := s.seen[onlineKey(p)]
		return ok && time.Since(at) < bluetoothSeenFor, done
	case "tcp":
		defer close(done)
		c, err := net.DialTimeout("tcp", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)), 2*time.Second)
		if err != nil {
			return false, done
		}
		return c.Close() == nil, done
	}
	close(done)
	return false, done
}
