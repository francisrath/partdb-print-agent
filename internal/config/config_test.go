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
instances:
  - id: home
    base_url: https://partdb.example.org
    token: tcp_fromfile
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

	t.Setenv("PDB_PRINT_TOKEN", "")
	if in, err := c.Instance(""); err != nil || in.Token != "tcp_fromfile" {
		t.Errorf("instance: %+v %v", in, err)
	}
	t.Setenv("PDB_PRINT_TOKEN", "tcp_env")
	if in, err := c.Instance("home"); err != nil || in.Token != "tcp_env" {
		t.Errorf("env token: %+v %v", in, err)
	}
	if _, err := c.Instance("nope"); err == nil {
		t.Error("unknown instance accepted")
	}
	if c.Instances[0].Token != "tcp_fromfile" {
		t.Error("Instance() wrote the env token into the config")
	}

	empty := &Config{}
	t.Setenv("PDB_PRINT_URL", "https://env.example.org")
	if in, err := empty.Instance(""); err != nil || in.BaseURL != "https://env.example.org" || in.Token != "tcp_env" {
		t.Errorf("env instance: %+v %v", in, err)
	}
	if _, err := empty.Instance("typo"); err == nil {
		t.Error("explicit unknown id resolved to the env instance")
	}
	t.Setenv("PDB_PRINT_URL", "env.example.org")
	if _, err := empty.Instance(""); err == nil {
		t.Error("bad PDB_PRINT_URL accepted")
	}
	t.Setenv("PDB_PRINT_URL", "http://partdb.lan")
	if _, err := empty.Instance(""); err == nil {
		t.Error("plain-http PDB_PRINT_URL accepted without PDB_PRINT_ALLOW_HTTP")
	}
	t.Setenv("PDB_PRINT_ALLOW_HTTP", "1")
	if _, err := empty.Instance(""); err != nil {
		t.Errorf("PDB_PRINT_ALLOW_HTTP: %v", err)
	}

	for name, bad := range map[string]string{
		"no id":      "printers: [{driver: brother-ql, transport: {kind: tcp, host: x}}]",
		"bad driver": "printers: [{id: a, driver: zpl, transport: {kind: tcp, host: x}}]",
		"no host":    "printers: [{id: a, driver: brother-ql, transport: {kind: tcp}}]",
		"duplicate":  "printers: [{id: a, driver: brother-ql, transport: {kind: tcp, host: x}}, {id: a, driver: brother-ql, transport: {kind: tcp, host: x}}]",
		"bad yaml":   "printers: [",
		"bad url":    "instances: [{id: h, base_url: partdb.local}]",
		"plain http": "instances: [{id: h, base_url: http://partdb.lan}]",
	} {
		if _, err := Load(write(bad), true); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPlainHTTP(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://partdb.lan":    true,
		"http://partdb.lan":     false,
		"http://127.0.0.1:8080": true,
		"http://localhost:8080": true,
		"http://[::1]:8080":     true,
	} {
		in := Instance{ID: "x", BaseURL: url}
		if err := in.validate(); (err == nil) != ok {
			t.Errorf("%s: %v", url, err)
		}
		in.AllowHTTP = true
		if err := in.validate(); err != nil {
			t.Errorf("%s with allow_http: %v", url, err)
		}
	}
}
