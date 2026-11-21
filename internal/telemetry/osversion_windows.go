package telemetry

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// osVersion is "windows 10", "windows 11" (build 22000 and up) or "windows server", with the build number.
func osVersion() string {
	v := windows.RtlGetVersion()
	name := "windows 10"
	switch {
	case v.ProductType != 1: // VER_NT_WORKSTATION
		name = "windows server"
	case v.BuildNumber >= 22000:
		name = "windows 11"
	}
	if v.MajorVersion != 10 {
		return "windows"
	}
	return fmt.Sprintf("%s build %d", name, v.BuildNumber)
}
