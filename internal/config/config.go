// Package config loads the agent's YAML config (architecture §5.1).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	LogLevel  string     `yaml:"log_level"` // debug|info|warn|error
	Instances []Instance `yaml:"instances"`
	Printers  []Printer  `yaml:"printers"`
}

// Instance is a Part-DB server.
type Instance struct {
	ID      string `yaml:"id"`
	BaseURL string `yaml:"base_url"` // https://partdb.example.org
	// Token is a Read-Only API token. PDB_PRINT_TOKEN overrides it.
	// ponytail: plain config field; OS keychain storage comes with the web UI setup.
	Token string `yaml:"token"`
	// AllowHTTP permits a plain-http base_url on a non-loopback host. The token then
	// crosses the network unencrypted; prefer https.
	AllowHTTP bool `yaml:"allow_http,omitempty"`
}

type Printer struct {
	ID        string    `yaml:"id"`
	Name      string    `yaml:"name"`
	Driver    string    `yaml:"driver"` // brother-ql
	Model     string    `yaml:"model"`  // QL-810W
	Transport Transport `yaml:"transport"`
	Media     string    `yaml:"media"`  // dk-22210; empty = use what the printer reports
	Rotate    string    `yaml:"rotate"` // auto (default) | 0 | 90 | 180 | 270
	Cut       string    `yaml:"cut"`    // per_label (default) | last | none
	Threshold uint8     `yaml:"threshold"`
}

type Transport struct {
	Kind string `yaml:"kind"` // tcp
	Host string `yaml:"host"`
	Port int    `yaml:"port"` // default 9100
}

// DefaultPath is <user config dir>/pdb-print/config.yaml.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pdb-print", "config.yaml"), nil
}

// Load reads path. A missing file yields an empty config unless mustExist.
func Load(path string, mustExist bool) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && !mustExist {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	ids := map[string]bool{}
	for i, in := range c.Instances {
		if in.ID == "" || in.BaseURL == "" {
			return fmt.Errorf("instances[%d]: id and base_url are required", i)
		}
		if ids[in.ID] {
			return fmt.Errorf("instance %q defined twice", in.ID)
		}
		ids[in.ID] = true
		if err := in.validate(); err != nil {
			return fmt.Errorf("instance %q: %w", in.ID, err)
		}
	}
	seen := map[string]bool{}
	for i := range c.Printers {
		p := &c.Printers[i]
		if p.ID == "" {
			return fmt.Errorf("printers[%d]: id is required", i)
		}
		if seen[p.ID] {
			return fmt.Errorf("printer %q defined twice", p.ID)
		}
		seen[p.ID] = true
		if p.Driver != "brother-ql" {
			return fmt.Errorf("printer %q: unsupported driver %q (MVP supports brother-ql)", p.ID, p.Driver)
		}
		if p.Transport.Kind != "tcp" || p.Transport.Host == "" {
			return fmt.Errorf("printer %q: transport must be kind tcp with a host", p.ID)
		}
		if p.Transport.Port == 0 {
			p.Transport.Port = 9100
		}
	}
	return nil
}

// Instance returns a copy of the instance with id (or the only one when id is empty)
// with its token resolved. With no instances configured, PDB_PRINT_URL defines one ("env").
func (c *Config) Instance(id string) (*Instance, error) {
	var in Instance
	switch {
	case len(c.Instances) == 0 && os.Getenv("PDB_PRINT_URL") != "" && (id == "" || id == "env"):
		in = Instance{ID: "env", BaseURL: os.Getenv("PDB_PRINT_URL"), AllowHTTP: os.Getenv("PDB_PRINT_ALLOW_HTTP") == "1"}
		if err := in.validate(); err != nil {
			return nil, fmt.Errorf("PDB_PRINT_URL: %w", err)
		}
	case id == "" && len(c.Instances) == 1:
		in = c.Instances[0]
	case id == "":
		return nil, fmt.Errorf("%d Part-DB instances configured: pick one with --instance", len(c.Instances))
	default:
		found := false
		for _, i := range c.Instances {
			if i.ID == id {
				in, found = i, true
			}
		}
		if !found {
			return nil, fmt.Errorf("no instance %q in config", id)
		}
	}
	if t := os.Getenv("PDB_PRINT_TOKEN"); t != "" {
		in.Token = t
	}
	if in.Token == "" {
		return nil, fmt.Errorf("instance %q has no API token: set token in the config or PDB_PRINT_TOKEN", in.ID)
	}
	return &in, nil
}

func (in *Instance) validate() error {
	u, err := url.Parse(in.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("base_url must be an http(s) URL")
	}
	if u.Scheme == "http" && !in.AllowHTTP && !IsLoopback(u.Hostname()) {
		return errors.New("base_url uses plain http, which would send the API token unencrypted: use https, or set allow_http: true")
	}
	return nil
}

// IsLoopback reports whether host is localhost or a loopback IP.
func IsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) Printer(id string) (*Printer, error) {
	for i := range c.Printers {
		if c.Printers[i].ID == id {
			return &c.Printers[i], nil
		}
	}
	return nil, fmt.Errorf("no printer %q in config", id)
}
