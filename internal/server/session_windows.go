package server

import "github.com/francisrath/pdb-print/internal/wininstall"

// keyOwnerOK checks the owner of the session key and pair-request files: Administrators or SYSTEM.
var keyOwnerOK = wininstall.OwnedByAdmins
