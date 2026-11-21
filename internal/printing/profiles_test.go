package printing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/pdfgen"
)

// fakePartDB has profiles 1 (12×16 part lid), 2 (29×46 part bag), 3 (54×17 part, landscape)
// and 4 (29×84 storage location, but no storage locations to label).
func fakePartDB(t *testing.T) (*httptest.Server, *int) {
	sizes := map[float64][2]float64{1: {12, 16}, 2: {29, 46}, 3: {54, 17}}
	generated := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/label_profiles":
			switch r.URL.Query().Get("options.supported_element") {
			case "part":
				w.Write([]byte(`{"hydra:member":[{"@id":"/api/label_profiles/1","name":"lid"},{"@id":"/api/label_profiles/2","name":"bag"},{"@id":"/api/label_profiles/3","name":"ic"}]}`))
			case "storelocation":
				w.Write([]byte(`{"hydra:member":[{"@id":"/api/label_profiles/4","name":"drawer"}]}`))
			default:
				w.Write([]byte(`{"hydra:member":[]}`))
			}
		case "/api/parts":
			w.Write([]byte(`{"hydra:member":[{"@id":"/api/parts/32"}]}`))
		case "/api/storage_locations":
			w.Write([]byte(`{"hydra:member":[]}`))
		case "/api/labels/generate":
			var body struct{ ProfileID float64 }
			json.NewDecoder(r.Body).Decode(&body)
			generated++
			sz := sizes[body.ProfileID]
			w.Write(pdfgen.Page(sz[0], sz[1], pdfgen.Text("x")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &generated
}

func TestFittingProfiles(t *testing.T) {
	srv, generated := fakePartDB(t)
	t.Setenv("PDB_PRINT_TOKEN", "tcp_x")
	cfg := &config.Config{Instances: []config.Instance{{ID: "pdb", BaseURL: srv.URL}}}
	s := service(t, nil)
	ctx := context.Background()
	for media, want := range map[string]string{
		"dk-22214": "fits profiles: 1 lid",
		"dk-22210": "fits profiles: 2 bag",
		"dk-11204": "fits profiles: 3 ic",
		"dk-22205": "no Part-DB label profile fits 62 mm tape",
	} {
		m, _ := devicedb.Default().Media(media)
		if got := s.FittingProfiles(ctx, cfg, &m); got != want {
			t.Errorf("%s: %q, want %q", media, got, want)
		}
	}
	if *generated != 3 {
		t.Errorf("%d sample labels generated; sizes should be measured once per profile", *generated)
	}
	if got := s.FittingProfiles(ctx, &config.Config{}, nil); got != "" {
		t.Errorf("no instance: %q", got)
	}
}
