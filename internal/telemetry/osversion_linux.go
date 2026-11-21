package telemetry

func osVersion() string { return linuxRelease("/etc/os-release") }
