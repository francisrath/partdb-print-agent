package server

import "net/http"

// Draft is a wizard session's printer setup until Finish writes it to the config.
type Draft struct{}

// setupRoutes registers the wizard's pages on mux (LAN and loopback).
func (s *Server) setupRoutes(mux *http.ServeMux) {}
