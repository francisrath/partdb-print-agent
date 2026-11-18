package printing

import (
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
	s.mu.Lock()
	st, ok := s.online[p.ID]
	s.mu.Unlock()
	if ok && time.Since(st.at) < onlineFor {
		return st.ok
	}
	lock := s.printerLock(p.ID)
	if !lock.TryLock() {
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
	s.online[p.ID] = onlineState{time.Now(), up}
	s.mu.Unlock()
	return up
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
		f, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err != nil {
			return false
		}
		return f.Close() == nil
	case "tcp":
		c, err := net.DialTimeout("tcp", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)), 2*time.Second)
		if err != nil {
			return false
		}
		return c.Close() == nil
	}
	return false
}
