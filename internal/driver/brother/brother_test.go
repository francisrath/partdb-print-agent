package brother

import (
	"testing"

	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
)

// Every Brother model in the device database must build a driver (catches a dialect typo
// in the YAML before anyone prints).
func TestEveryBrotherModelBuilds(t *testing.T) {
	for _, m := range devicedb.Default().Models("brother") {
		if _, err := driver.New(m, driver.Options{}); err != nil {
			t.Errorf("%s: %v", m.ID, err)
		}
	}
}
