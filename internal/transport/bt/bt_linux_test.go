package bt

import "golang.org/x/sys/unix"

var osErrnos = linuxErrnos

var osCases = []osCase{
	{unix.EHOSTDOWN, true, false},
	{unix.ECONNREFUSED, true, false},
	{unix.ETIMEDOUT, true, false},
	{unix.EHOSTUNREACH, false, true},
	{unix.EADDRNOTAVAIL, false, true},
	{unix.ENODEV, false, true},
	{unix.EAFNOSUPPORT, false, true},
	{unix.EACCES, true, false},
	{unix.EPROTO, false, false},
}
