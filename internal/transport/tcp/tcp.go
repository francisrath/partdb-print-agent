// Package tcp is the raw TCP (port 9100) transport.
package tcp

import (
	"context"
	"net"
	"time"
)

type Transport struct {
	Addr        string        // host:port
	ReadTimeout time.Duration // per Read; 0 = 2s
	conn        net.Conn
}

func New(addr string) *Transport { return &Transport{Addr: addr} }

func (t *Transport) Kind() string { return "tcp" }

func (t *Transport) Open(ctx context.Context) error {
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		return err
	}
	t.conn = c
	return nil
}

func (t *Transport) Write(p []byte) (int, error) {
	if err := t.conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return 0, err
	}
	return t.conn.Write(p)
}

// Read returns os.ErrDeadlineExceeded when the printer says nothing within ReadTimeout.
func (t *Transport) Read(p []byte) (int, error) {
	to := t.ReadTimeout
	if to == 0 {
		to = 2 * time.Second
	}
	if err := t.conn.SetReadDeadline(time.Now().Add(to)); err != nil {
		return 0, err
	}
	return t.conn.Read(p)
}

func (t *Transport) Close() error {
	if t.conn == nil {
		return nil
	}
	err := t.conn.Close()
	t.conn = nil
	return err
}
