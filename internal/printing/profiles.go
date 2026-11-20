package printing

import (
	"context"
	"fmt"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/partdb"
	"github.com/francisrath/partdb-print-agent/internal/render"
)

// profileSizeTTL bounds how long a measured profile size is trusted (profiles get edited).
const profileSizeTTL = 10 * time.Minute

type profileSize struct {
	w, h float64
	at   time.Time
}

// ProfilesFitting lists the Part-DB label profiles whose labels fit m. Part-DB's API doesn't
// expose a profile's size, so each is measured from a sample label (one element of its type),
// and remembered for 10 minutes. Profiles with nothing to label are left out.
func (s *Service) ProfilesFitting(ctx context.Context, c *partdb.Client, m *devicedb.Media) ([]partdb.Profile, error) {
	profiles, err := c.Profiles(ctx)
	if err != nil {
		return nil, err
	}
	var out []partdb.Profile
	for _, p := range profiles {
		size, err := s.profileSize(ctx, c, p)
		if err != nil {
			return out, fmt.Errorf("profile %d: %w", p.ID, err)
		}
		if size.w > 0 && m.Fits(size.w, size.h) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Service) profileSize(ctx context.Context, c *partdb.Client, p partdb.Profile) (profileSize, error) {
	key := fmt.Sprintf("%s#%d", c.BaseURL, p.ID)
	s.mu.Lock()
	size, ok := s.profileSizes[key]
	s.mu.Unlock()
	if ok && time.Since(size.at) < profileSizeTTL {
		return size, nil
	}
	size = profileSize{at: time.Now()}
	id, err := c.SampleID(ctx, p.Element)
	if err != nil || id == 0 {
		return size, err
	}
	pdf, err := c.GenerateLabels(ctx, p.ID, fmt.Sprint(id), p.Element)
	if err != nil {
		return size, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.r == nil {
		if s.r, err = render.New(); err != nil {
			return size, err
		}
	}
	if size.w, size.h, err = s.r.PageSizeMM(pdf); err != nil {
		return size, err
	}
	if s.profileSizes == nil {
		s.profileSizes = map[string]profileSize{}
	}
	s.profileSizes[key] = size
	return size, nil
}
