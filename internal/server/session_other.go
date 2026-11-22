//go:build !windows

package server

// keyOwnerOK checks the owner of the session key and pair-request files; nil: no check (PairLoopback is the Windows service).
var keyOwnerOK func(path string) (bool, error)
