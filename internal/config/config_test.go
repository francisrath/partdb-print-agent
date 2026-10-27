package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	c, err := Load(filepath.Join(dir, "missing.yaml"), false)
	if err != nil || len(c.Printers) != 0 {
		t.Fatalf("missing optional file: %v %v", c, err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml"), true); err == nil {
		t.Error("missing explicit file accepted")
	}

	c, err = Load(write(`
printers:
  - id: ql810
    driver: brother-ql
    model: QL-810W
    transport: {kind: tcp, host: 192.168.1.50}
    media: dk-22210
`), true)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Printer("ql810")
	if err != nil || p.Transport.Port != 9100 || p.Media != "dk-22210" {
		t.Errorf("got %+v, %v", p, err)
	}

	for name, bad := range map[string]string{
		"no id":      "printers: [{driver: brother-ql, transport: {kind: tcp, host: x}}]",
		"bad driver": "printers: [{id: a, driver: zpl, transport: {kind: tcp, host: x}}]",
		"no host":    "printers: [{id: a, driver: brother-ql, transport: {kind: tcp}}]",
		"duplicate":  "printers: [{id: a, driver: brother-ql, transport: {kind: tcp, host: x}}, {id: a, driver: brother-ql, transport: {kind: tcp, host: x}}]",
		"bad yaml":   "printers: [",
	} {
		if _, err := Load(write(bad), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
