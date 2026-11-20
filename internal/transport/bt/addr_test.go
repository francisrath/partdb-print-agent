package bt

import "testing"

func TestParseAddr(t *testing.T) {
	tests := []struct {
		in   string
		want [6]byte
		ok   bool
	}{
		{"AA:BB:CC:DD:EE:FF", [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}, true},
		{"aa:bb:cc:dd:ee:0f", [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x0F}, true},
		{"AA:BB", [6]byte{}, false},
		{"GG:BB:CC:DD:EE:FF", [6]byte{}, false},
		{"AA-BB-CC-DD-EE-FF", [6]byte{}, false},
		{"AA:BB:CC:DD:EE:FF:00", [6]byte{}, false},
		{"", [6]byte{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseAddr(tt.in)
			if (err == nil) != tt.ok || got != tt.want {
				t.Fatalf("ParseAddr(%q) = %X, %v", tt.in, got, err)
			}
			if tt.ok {
				if s := FormatAddr(got); s != "AA:BB:CC:DD:EE:"+s[15:] || len(s) != 17 {
					t.Fatalf("FormatAddr = %q", s)
				}
				if back, _ := ParseAddr(FormatAddr(got)); back != got {
					t.Fatalf("round trip %X -> %X", got, back)
				}
			}
		})
	}
	if s := FormatAddr([6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x0F}); s != "AA:BB:CC:DD:EE:0F" {
		t.Errorf("FormatAddr = %q", s)
	}
}

func TestByteOrders(t *testing.T) {
	a := [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	if got := rfcommAddr(a); got != [6]byte{0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA} {
		t.Errorf("rfcommAddr = %X", got)
	}
	if got := bthAddr(a); got != 0xAABBCCDDEEFF {
		t.Errorf("bthAddr = %#x", got)
	}
	if got := fromBth(0xAABBCCDDEEFF); got != a {
		t.Errorf("fromBth = %X", got)
	}
}
