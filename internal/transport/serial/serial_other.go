//go:build !linux && !darwin && !windows

package serial

import "errors"

var errUnsupported = errors.New("serial ports are supported on Linux, macOS and Windows")

func openPort(string, Line) (port, error) { return nil, errUnsupported }

// Ports lists nothing here.
func Ports() []string { return nil }

// Present is always false here.
func Present(string) bool { return false }
