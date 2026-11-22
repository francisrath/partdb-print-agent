package server

import "github.com/francisrath/pdb-print/internal/wininstall"

// keyOwnerOK checks the session key file's owner: Administrators or SYSTEM.
var keyOwnerOK = wininstall.OwnedByAdmins
