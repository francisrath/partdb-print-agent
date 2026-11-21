package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// counts is what the agent remembers between reports (a small JSON file next to the config).
type counts struct {
	Since  string         `json:"since"` // ISO week counting started
	Sent   string         `json:"sent"`  // ISO week of the last report sent
	Jobs   map[string]int `json:"jobs"`
	Labels map[string]int `json:"labels"`
	Errors map[string]int `json:"errors"`
}

// Store keeps the counters on disk so a restart doesn't lose a week's numbers.
type Store struct {
	Path string
	Now  func() time.Time // nil = time.Now

	mu sync.Mutex
	c  counts
}

// Open reads the counters at path; a missing or unreadable file starts fresh (they are only statistics).
func Open(path string) *Store {
	s := &Store{Path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.c)
	}
	return s
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Week is t's ISO week: 2026-W47.
func Week(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// Job counts one print job: its source, and on success the pages printed on model, else its error code.
func (s *Store) Job(source, model string, pages int, errCode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start()
	add(&s.c.Jobs, source, 1)
	if errCode != "" {
		add(&s.c.Errors, errCode, 1)
	} else if model != "" {
		add(&s.c.Labels, model, pages)
	}
	s.save()
}

func add(m *map[string]int, k string, n int) {
	if *m == nil {
		*m = map[string]int{}
	}
	(*m)[k] += n
}

// start begins counting this week if nothing was counted yet (caller holds mu).
func (s *Store) start() {
	if s.c.Since == "" {
		s.c.Since = Week(s.now())
	}
}

// due: counting started in an earlier week and nothing was sent this week.
func (s *Store) due() (counts, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start()
	w := Week(s.now())
	return s.snapshot(), s.c.Since < w && s.c.Sent != w
}

func (s *Store) snapshot() counts {
	c := s.c
	c.Jobs, c.Labels, c.Errors = clone(c.Jobs), clone(c.Labels), clone(c.Errors)
	return c
}

func clone(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// reset starts a new count this week; sent records that a report went out.
func (s *Store) reset(sent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := Week(s.now())
	last := s.c.Sent
	if sent {
		last = w
	}
	s.c = counts{Since: w, Sent: last}
	s.save()
}

// save writes the file atomically; failures only cost statistics (caller holds mu).
func (s *Store) save() {
	if s.Path == "" {
		return
	}
	b, _ := json.Marshal(s.c)
	tmp := s.Path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return
	}
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.Path)
	}
}
