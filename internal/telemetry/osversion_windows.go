package telemetry

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// osVersion is "windows 10" or "windows 11" (build 22000 and up), with the build number.
func osVersion() string {
	v := windows.RtlGetVersion()
	name := "windows 10"
	if v.MajorVersion == 10 && v.BuildNumber >= 22000 {
		name = "windows 11"
	} else if v.MajorVersion != 10 {
		name = fmt.Sprintf("windows %d.%d", v.MajorVersion, v.MinorVersion)
	}
	return fmt.Sprintf("%s build %d", name, v.BuildNumber)
}
