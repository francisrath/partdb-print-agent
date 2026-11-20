package bt

import "golang.org/x/sys/windows"

var osErrnos = winErrnos

var osCases = []osCase{
	{windows.WSAETIMEDOUT, true, false},
	{windows.WSAECONNREFUSED, true, false},
	{windows.WSAEHOSTDOWN, true, false},
	{windows.WSAEAFNOSUPPORT, false, true},
	{windows.WSAENETDOWN, false, true},
	{windows.WSAEACCES, false, false},
}
