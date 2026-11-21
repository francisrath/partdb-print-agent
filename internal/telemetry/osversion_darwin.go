package telemetry

import (
	"strings"

	"golang.org/x/sys/unix"
)

// osVersion is the macOS major version: "macos 15".
func osVersion() string {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return "macos"
	}
	major, _, _ := strings.Cut(v, ".")
	return "macos " + major
}
