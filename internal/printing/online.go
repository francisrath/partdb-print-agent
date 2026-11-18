package printing

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
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
// on its print port within 2 s is (nothing is sent). Results are cached for 30 s; checks of
// one printer run one at a time and share their result.
func (s *Service) Online(p *config.Printer) bool {
	key := onlineKey(p)
	if up, ok := s.cachedOnline(key); ok {
		return up
	}
	probe := s.lockFor(&s.probes, p.ID)
	probe.Lock() // a check already running for this printer: wait for its result
	defer probe.Unlock()
	if up, ok := s.cachedOnline(key); ok {
		return up
	}
	lock := s.printerLock(p.ID)
	if !lock.TryLock() {
		return true // only jobs hold it now: printing right now
	}
	up, done := s.check(p) // under the printer lock: a USB open can't collide with a starting job
	s.mu.Lock()
	if s.online == nil {
		s.online = map[string]onlineState{}
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
	return reachable(p)
}

// onlineKey keys the cache by printer and address, so an edited address is checked afresh.
func onlineKey(p *config.Printer) string {
	t := p.Transport
	return fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s", p.ID, t.Kind, t.Host, t.Port, t.Device, t.USBID, t.Serial)
}

func reachable(p *config.Printer) (bool, <-chan struct{}) {
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
		opened := make(chan bool, 1)
		go func() { // usblp opens don't block, but a check must never hang a heartbeat
			defer close(done)
			f, err := os.OpenFile(dev, os.O_RDWR, 0)
			opened <- err == nil && f.Close() == nil
		}()
		select {
		case ok := <-opened:
			return ok, done
		case <-time.After(2 * time.Second):
			return false, done
		}
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
