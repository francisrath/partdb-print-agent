package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestLoad(t *testing.T) {
	keyring.MockInit()
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
		t.Error("Instance() leaked the env token into the config")
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

	// each case has valid parts except the one it tests; want is part of the expected error
	for name, tc := range map[string]struct{ yaml, want string }{
		"no id":                      {"printers: [{model: QL-810W, transport: {kind: tcp, host: x}}]", "id is required"},
		"driver field (removed)":     {"printers: [{id: a, driver: brother, model: QL-810W, transport: {kind: tcp, host: x}}]", "driver"},
		"no host":                    {"printers: [{id: a, model: QL-810W, transport: {kind: tcp}}]", "needs a host"},
		"duplicate":                  {"printers: [{id: a, model: QL-810W, transport: {kind: tcp, host: x}}, {id: a, model: QL-810W, transport: {kind: tcp, host: x}}]", "defined twice"},
		"no model":                   {"printers: [{id: a, transport: {kind: usb, device: /dev/usb/lp0}}]", "unknown model"},
		"bad yaml":                   {"printers: [", "yaml"},
		"bad url":                    {"instances: [{id: h, base_url: partdb.local}]", "base_url"},
		"plain http":                 {"instances: [{id: h, base_url: http://partdb.lan}]", "http"},
		"bad route":                  {"routes: [{profile: 2, printer: nope}]", "routes[0]"},
		"hot folder without printer": {"hot_folder: {enabled: true}", "hot_folder"},
		"hot folder bad pattern":     {"hot_folder: {pattern: \"[\"}", "pattern"},
		"pt over tcp":                {"printers: [{id: a, model: PT-9500PC, transport: {kind: tcp, host: x}}]", "connects over usb"},
		"usb without device":         {"printers: [{id: a, model: PT-9500PC, transport: {kind: usb}}]", "device or a usb_id"},
	} {
		_, err := Load(write(tc.yaml), true)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
	c, err = Load(write("printers: [{id: pt, model: PT-9500PC, transport: {kind: usb, device: /dev/usb/lp0}}]"), true)
	if err != nil || c.Printers[0].Transport.Device != "/dev/usb/lp0" {
		t.Errorf("brother-pt over usb: %v", err)
	}
}

func TestKeychainAndSave(t *testing.T) {
	keyring.MockInit()
	t.Setenv("PDB_PRINT_TOKEN", "")
	t.Setenv("PDB_PRINT_URL", "")
	c := &Config{
		Instances: []Instance{{ID: "home", BaseURL: "https://partdb.example.org", PairingKey: "ab"}},
		Printers:  []Printer{{ID: "ql", Model: "QL-810W", Transport: Transport{Kind: "tcp", Host: "h", Port: 9100}}},
		Routes:    []Route{{Profile: 2, Printer: "ql", Name: "bag"}},
	}
	if _, err := c.Instance("home"); err == nil {
		t.Error("instance without any token accepted")
	}
	if err := keyring.Set(KeyringService, "home", "tcp_keychain"); err != nil {
		t.Fatal(err)
	}
	if in, err := c.Instance("home"); err != nil || in.Token != "tcp_keychain" {
		t.Errorf("keychain token: %+v %v", in, err)
	}

	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "tcp_") {
		t.Errorf("token written to config:\n%s", b)
	}
	got, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := got.Route("", 2); !ok || r.Printer != "ql" || got.Instances[0].PairingKey != "ab" {
		t.Errorf("round trip: %+v", got)
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

func TestRoutePrefersExactInstance(t *testing.T) {
	c := &Config{Routes: []Route{
		{Profile: 2, Printer: "default-ql"},                    // any instance
		{Instance: "branch", Profile: 2, Printer: "branch-ql"}, // only "branch"
		{Instance: "home", Profile: 3, Printer: "home-ql"},
	}}
	for _, tc := range []struct {
		instance string
		profile  int
		want     string
	}{
		{"branch", 2, "branch-ql"}, // exact beats the earlier catch-all
		{"home", 2, "default-ql"},
		{"", 2, "default-ql"},
		{"home", 3, "home-ql"},
		{"branch", 3, ""}, // another instance's route doesn't apply
	} {
		r, ok := c.Route(tc.instance, tc.profile)
		got := ""
		if ok {
			got = r.Printer
		}
		if got != tc.want {
			t.Errorf("Route(%q, %d) = %q, want %q", tc.instance, tc.profile, got, tc.want)
		}
	}
}

func TestHotFolderPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"":            filepath.Join(home, "Downloads"),
		"~/Labels":    filepath.Join(home, "Labels"),
		"/tmp/labels": "/tmp/labels",
	} {
		if got, err := (HotFolder{Dir: in}).Path(); err != nil || got != want {
			t.Errorf("Path(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestValidateModelTransport(t *testing.T) {
	write := func(y string) string {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, y := range map[string]string{
		"ql over tcp":          "printers: [{id: a, model: QL-810W, transport: {kind: tcp, host: x}}]",
		"ql by usb id":         "printers: [{id: a, model: QL-810W, transport: {kind: usb, usb_id: \"04f9:209c\", serial: 000G5G313834}}]",
		"ql-800 by usb path":   "printers: [{id: a, model: QL-800, transport: {kind: usb, device: /dev/usb/lp0}}]",
		"pt media":             "printers: [{id: a, model: PT-9500PC, transport: {kind: usb, usb_id: \"04f9:200f\"}, media: tze-24}]",
		"bt by address":        "printers: [{id: a, model: RG-MTP80B, transport: {kind: bt, address: \"aa:bb:cc:dd:ee:ff\"}}]",
		"bt by name":           "printers: [{id: a, model: RG-MTP80B, transport: {kind: bt, name: RG-MTP80B, channel: 30}}]",
		"ble by name":          "printers: [{id: a, model: MX10, transport: {kind: ble, name: MX10}, darkness: dark}]",
		"ble address and name": "printers: [{id: a, model: MX10, transport: {kind: ble, address: \"AA:BB:CC:DD:EE:FF\", name: MX10}}]",
		"darkness light":       "printers: [{id: a, model: MX10, transport: {kind: ble, name: MX10}, darkness: light}]",
		"darkness normal":      "printers: [{id: a, model: MX10, transport: {kind: ble, name: MX10}, darkness: normal}]",
		"ble by address":       "printers: [{id: a, model: MX10, transport: {kind: ble, address: \"AA:BB:CC:DD:EE:FF\"}, media: paper-57}]",
		"epl black mark":       "printers: [{id: a, model: LP2844, transport: {kind: usb, usb_id: \"0a5f:0009\"}, media: label-102x51, black_mark_mm: 4, darkness: dark}]",
		"serial defaults":      "printers: [{id: a, model: LP2844, transport: {kind: serial, device: /dev/ttyUSB0}}]",
		"serial line":          "printers: [{id: a, model: GK420d, transport: {kind: serial, device: COM3, baud: 115200, framing: 8E2, flow: rtscts}}]",
		"media auto":           "printers: [{id: a, model: QL-810W, transport: {kind: tcp, host: x}, media: auto}]",
		"allow-list":           "printers: [{id: a, model: QL-810W, transport: {kind: tcp, host: x}, media: dk-22210, media_allowed: [dk-22210, dk-22214, dk-11204]}]",
	} {
		if _, err := Load(write(y), true); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for y, want := range map[string][]string{
		"printers: [{id: a, model: QL-9000, transport: {kind: tcp, host: x}}]":                                             {"QL-9000", "QL-810W", "PT-9500PC"},
		"printers: [{id: a, model: QL-800, transport: {kind: tcp, host: x}}]":                                              {"QL-800", "tcp"},
		"printers: [{id: a, model: QL-810W, transport: {kind: tcp, host: x}, media: tze-24}]":                              {"tze-24", "QL-810W"},
		"printers: [{id: a, model: QL-810W, transport: {kind: usb, device: /dev/usb/lp0, usb_id: \"04f9:209c\"}}]":         {"device", "usb_id"},
		"printers: [{id: a, model: QL-810W, transport: {kind: usb, usb_id: \"4f9:209c\"}}]":                                {"usb_id", "4f9:209c"},
		"printers: [{id: a, model: QL-810W, transport: {kind: usb, device: /dev/usb/lp0, serial: X1}}]":                    {"serial", "usb_id"},
		"printers: [{id: a, model: RG-MTP80B, transport: {kind: bt}}]":                                                     {"bt", "address", "name"},
		"printers: [{id: a, model: RG-MTP80B, transport: {kind: bt, address: \"AA:BB\"}}]":                                 {"address", "AA:BB"},
		"printers: [{id: a, model: RG-MTP80B, transport: {kind: bt, name: X, channel: 31}}]":                               {"channel", "31"},
		"printers: [{id: a, model: MX10, transport: {kind: ble}}]":                                                         {"ble", "address", "name"},
		"printers: [{id: a, model: QL-810W, transport: {kind: ble, name: MX10}}]":                                          {"QL-810W", "ble"},
		"printers: [{id: a, model: QL-810W, transport: {kind: tcp, host: x}, darkness: dark}]":                             {"darkness", "QL-810W"},
		"printers: [{id: a, model: MX10, transport: {kind: ble, name: MX10}, darkness: extra}]":                            {"darkness", "extra"},
		"printers: [{id: a, model: QL-810W, transport: {kind: tcp, host: x}, black_mark_mm: 3}]":                           {"black mark", "QL-810W"},
		"printers: [{id: a, model: LP2844, transport: {kind: tcp, host: x}, black_mark_mm: -1}]":                           {"black_mark_mm", "-1"},
		"printers: [{id: a, model: LP2742, transport: {kind: usb, device: /dev/usb/lp0}}]":                                 {"LP2742", "usb"},
		"printers: [{id: a, model: LP2844, transport: {kind: serial}}]":                                                    {"serial", "device"},
		"printers: [{id: a, model: LP2844, transport: {kind: serial, device: /dev/ttyS0, baud: 9601}}]":                    {"baud", "9601"},
		"printers: [{id: a, model: LP2844, transport: {kind: serial, device: /dev/ttyS0, framing: 8X1}}]":                  {"framing", "8X1"},
		"printers: [{id: a, model: LP2844, transport: {kind: serial, device: /dev/ttyS0, framing: 7E1}}]":                  {"framing", "7E1"},
		"printers: [{id: a, model: LP2844, transport: {kind: serial, device: /dev/ttyS0, flow: dtr}}]":                     {"flow", "dtr"},
		"printers: [{id: a, model: QL-810W, transport: {kind: serial, device: /dev/ttyS0}}]":                               {"QL-810W", "serial"},
		"printers: [{id: a, model: QL-810W, transport: {kind: tcp, host: x}, media_allowed: [dk-22210, tze-24]}]":          {"tze-24", "QL-810W"},
		"printers: [{id: a, model: QL-810W, transport: {kind: tcp, host: x}, media: dk-22205, media_allowed: [dk-22210]}]": {"dk-22205", "media_allowed"},
	} {
		_, err := Load(write(y), true)
		if err == nil {
			t.Errorf("accepted: %s", y)
			continue
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q doesn't name %q", y, err, w)
			}
		}
	}
}

func TestBluetooth(t *testing.T) {
	for kind, want := range map[string]bool{"ble": true, "bt": true, "tcp": false, "usb": false, "": false} {
		if got := Bluetooth(kind); got != want {
			t.Errorf("Bluetooth(%q) = %v", kind, got)
		}
	}
}

func TestUpdateCheck(t *testing.T) {
	off := false
	for _, c := range []struct {
		cfg  Config
		want bool
	}{{Config{}, true}, {Config{UpdateCheck: &off}, false}} {
		if got := c.cfg.UpdateCheckOn(); got != c.want {
			t.Errorf("%+v: %v", c.cfg.UpdateCheck, got)
		}
	}
}

func TestSelfUpdateEnvOff(t *testing.T) {
	off, on := false, true
	for _, c := range []struct {
		self *bool
		env  string
		want bool
	}{
		{nil, "", true}, {&on, "", true}, {&off, "", false},
		{nil, "off", false}, {&on, "off", false}, {nil, "on", true},
	} {
		t.Setenv("PDB_PRINT_SELF_UPDATE", c.env)
		if got := (&Config{SelfUpdate: c.self}).SelfUpdateOn(); got != c.want {
			t.Errorf("self_update %v, env %q: %v", c.self, c.env, got)
		}
	}
}
