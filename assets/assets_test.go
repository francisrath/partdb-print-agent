package assets

import "testing"

func TestFaviconEmbedded(t *testing.T) {
	for _, f := range []string{"icon/web/favicon.svg", "icon/web/favicon.ico", "icon/web/apple-touch-icon.png"} {
		if b, err := Web.ReadFile(f); err != nil || len(b) == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
}
