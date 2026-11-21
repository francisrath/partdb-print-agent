package blebridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestBridgeFraming(t *testing.T) {
	tests := []struct {
		name    string
		typ     byte
		payload []byte
	}{
		{"open", TypeOpen, []byte(`{"name":"MX10"}`)},
		{"empty close", TypeClose, nil},
		{"large write", TypeWrite, bytes.Repeat([]byte{0x51, 0x78}, 40000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Write(&buf, tt.typ, tt.payload); err != nil {
				t.Fatal(err)
			}
			if got := buf.Len(); got != 5+len(tt.payload) {
				t.Fatalf("frame is %d bytes, want %d", got, 5+len(tt.payload))
			}
			frame := buf.Bytes()
			typ, p, err := Read(bytes.NewReader(frame))
			if err != nil || typ != tt.typ || !bytes.Equal(p, tt.payload) {
				t.Fatalf("round trip: %q %d bytes %v", typ, len(p), err)
			}
			for _, cut := range []int{1, 4, len(frame) - 1} {
				if cut >= len(frame) || cut == 0 {
					continue
				}
				if _, _, err := Read(bytes.NewReader(frame[:cut])); !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("truncated at %d: %v, want io.ErrUnexpectedEOF", cut, err)
				}
			}
		})
	}
	if _, _, err := Read(bytes.NewReader(nil)); err != io.EOF {
		t.Errorf("no frame: %v, want io.EOF", err)
	}
	if _, _, err := Read(bytes.NewReader([]byte{'W', 0xff, 0, 0, 0})); err == nil {
		t.Error("oversized length accepted")
	}
	if err := Write(io.Discard, TypeWrite, make([]byte, MaxFrame+1)); err == nil {
		t.Error("oversized write accepted")
	}
}

// Kind and Channel select an RFCOMM link; a request without them (an older client) is BLE.
func TestOpenRequestKind(t *testing.T) {
	in := OpenRequest{Kind: "rfcomm", Name: "RG-MLP80B", Address: "AA:BB:CC:DD:EE:FF", Channel: 3}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out OpenRequest
	if err := json.Unmarshal(b, &out); err != nil || out != in {
		t.Fatalf("round trip %s: %+v %v", b, out, err)
	}
	b, _ = json.Marshal(OpenRequest{Name: "MX10", Service: "0000ae30-0000-1000-8000-00805f9b34fb"})
	if bytes.Contains(b, []byte("kind")) || bytes.Contains(b, []byte("channel")) {
		t.Errorf("BLE request %s carries kind or channel", b)
	}
}

// A paired device not yet seen in an inquiry may lack its name and class: the inquiry fills them in,
// without overwriting what pairing already knows.
func TestMergeClassic(t *testing.T) {
	paired := []ClassicDevice{{Address: "AA:BB:CC:DD:EE:01"}, {Address: "AA:BB:CC:DD:EE:02", Name: "Bench", Class: 0x0680}}
	found := []ClassicDevice{{Address: "AA:BB:CC:DD:EE:01", Name: "RG-MLP80B", Class: 0x0680},
		{Address: "AA:BB:CC:DD:EE:02", Name: "RG-MLP80B"}, {Address: "AA:BB:CC:DD:EE:03", Name: "Phone", Class: 0x020C}}
	got := MergeClassic(paired, found)
	want := []ClassicDevice{{Address: "AA:BB:CC:DD:EE:01", Name: "RG-MLP80B", Class: 0x0680},
		{Address: "AA:BB:CC:DD:EE:02", Name: "Bench", Class: 0x0680}, {Address: "AA:BB:CC:DD:EE:03", Name: "Phone", Class: 0x020C}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
