//go:build !linux && !darwin && !windows

package telemetry

func osVersion() string { return "" }
