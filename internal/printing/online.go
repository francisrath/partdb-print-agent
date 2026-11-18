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
// on its print port within 2 s is (nothing is sent). Results are cached for 30 s.
func (s *Service) Online(p *config.Printer) bool {
	key := onlineKey(p)
	s.mu.Lock()
	st, ok := s.online[key]
	s.mu.Unlock()
	if ok && time.Since(st.at) < onlineFor {
		return st.ok
	}
	lock := s.printerLock(p.ID)
	if !lock.TryLock() {
		if ok {
			return st.ok // another check (or a job) holds it: keep the last known state
		}
		return true // printing a job right now
	}
	check := s.OnlineCheck
	if check == nil {
		check = reachable
	}
	up := check(p) // under the printer lock: a USB open can't collide with a starting job
	lock.Unlock()
	s.mu.Lock()
	if s.online == nil {
		s.online = map[string]onlineState{}
	}
	s.online[key] = onlineState{time.Now(), up}
	s.mu.Unlock()
	return up
}

// onlineKey keys the cache by printer and address, so an edited address is checked afresh.
func onlineKey(p *config.Printer) string {
	t := p.Transport
	return fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s", p.ID, t.Kind, t.Host, t.Port, t.Device, t.USBID, t.Serial)
}

func reachable(p *config.Printer) bool {
	t := p.Transport
	switch t.Kind {
	case "usb":
		dev := t.Device
		if t.USBID != "" {
			var err error
			if dev, err = usb.Find("", t.USBID, t.Serial); err != nil {
				return false
			}
		}
		opened := make(chan bool, 1)
		go func() { // usblp opens don't block, but a check must never hang a heartbeat
			f, err := os.OpenFile(dev, os.O_RDWR, 0)
			opened <- err == nil && f.Close() == nil
		}()
		select {
		case ok := <-opened:
			return ok
		case <-time.After(2 * time.Second):
			return false
		}
	case "tcp":
		c, err := net.DialTimeout("tcp", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)), 2*time.Second)
		if err != nil {
			return false
		}
		return c.Close() == nil
	}
	return false
}
