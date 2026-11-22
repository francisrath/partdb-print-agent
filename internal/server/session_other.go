//go:build !windows

package server

// keyOwnerOK checks the session key file's owner; nil: no check (PairLoopback is the Windows service).
var keyOwnerOK func(path string) (bool, error)
