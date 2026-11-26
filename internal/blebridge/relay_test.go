package blebridge

import (
	"bytes"
	"errors"
	"testing"
)

func frames(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	r := bytes.NewReader(b)
	for r.Len() > 0 {
		typ, p, err := Read(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(typ)+string(p))
	}
	return out
}

func TestRelay(t *testing.T) {
	var in bytes.Buffer
	_ = Write(&in, TypeOpen, []byte(`{"kind":"usb","device":"iokit:0x1"}`))
	_ = Write(&in, TypeWrite, []byte("ab"))
	_ = Write(&in, TypeWrite, []byte("bad"))
	_ = Write(&in, TypeClose, nil)
	_ = Write(&in, TypeWrite, []byte("after close"))
	req, err := ReadOpen(&in)
	if err != nil || req.Kind != KindUSB || req.Device != "iokit:0x1" {
		t.Fatalf("ReadOpen = %+v, %v", req, err)
	}
	var sent []string
	closed := false
	var out bytes.Buffer
	err = Relay(&in, &Frames{W: &out}, func() (Link, error) {
		return Link{MTU: 64, Send: func(p []byte) error {
			if string(p) == "bad" {
				return errors.New("stalled")
			}
			sent = append(sent, string(p))
			return nil
		}, Close: func() { closed = true }}, nil
	})
	got := frames(t, out.Bytes())
	want := []string{`K{"mtu":64}`, "K", "Estalled"}
	if err != nil || !closed || len(sent) != 1 || sent[0] != "ab" || len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("err %v closed %v sent %q frames %q", err, closed, sent, got)
	}

	out.Reset()
	if err := Relay(nil, &Frames{W: &out}, func() (Link, error) { return Link{}, errors.New("in use") }); err == nil {
		t.Fatal("open error not returned")
	}
	if got := frames(t, out.Bytes()); len(got) != 1 || got[0] != "Ein use" {
		t.Fatalf("open error frames %q", got)
	}
	if req, err := ReadOpen(&bytes.Buffer{}); req != nil || err != nil {
		t.Fatalf("empty input: %v %v", req, err)
	}
}
