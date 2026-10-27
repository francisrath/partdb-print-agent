// Package config loads the agent's YAML config (architecture §5.1).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	LogLevel string    `yaml:"log_level"` // debug|info|warn|error
	Printers []Printer `yaml:"printers"`
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

func (c *Config) Printer(id string) (*Printer, error) {
	for i := range c.Printers {
		if c.Printers[i].ID == id {
			return &c.Printers[i], nil
		}
	}
	return nil, fmt.Errorf("no printer %q in config", id)
}
