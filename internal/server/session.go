package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/francisrath/pdb-print/internal/wininstall"
)

// PairLoopback's sessions: a browser that typed the code gets a stateless cookie
// "<expiry, unix seconds>.<hex HMAC-SHA256 of the expiry>", keyed by the secret in SessionKeyFile,
// so it survives restarts. ponytail: no per-browser sign-out; deleting the key file signs all out.
const (
	sessionCookie = "pdb_session"
	sessionLife   = 30 * 24 * time.Hour
)

// sessionKey reads the key file; create makes a new key when there is none.
func (s *Server) sessionKey(create bool) ([]byte, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	key, err := os.ReadFile(s.SessionKeyFile)
	if err == nil && keyOwnerOK != nil { // (session_windows.go) a key someone else planted signs nothing; a new one replaces it
		if ok, oerr := keyOwnerOK(s.SessionKeyFile); !ok {
			id := "?"
			if fi, err := os.Stat(s.SessionKeyFile); err == nil {
				id = fmt.Sprint(fi.ModTime().UnixNano(), fi.Size())
			}
			if id != s.badKey { // each bad file once, not on every request
				s.badKey = id
				s.Log.Warn("session key not owned by Administrators or SYSTEM: ignored", "file", s.SessionKeyFile, "err", oerr)
			}
			key, err = nil, fmt.Errorf("%s: not owned by Administrators or SYSTEM: %w", s.SessionKeyFile, os.ErrNotExist)
		}
	}
	if errors.Is(err, os.ErrNotExist) && create {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err == nil {
			err = wininstall.WriteAdminOnly(s.SessionKeyFile, key)
		}
	}
	if err == nil && len(key) != 32 {
		err = fmt.Errorf("%s: not a session key (want 32 bytes)", s.SessionKeyFile)
	}
	return key, err
}

func sessionMAC(key []byte, exp string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("pdb-print session " + exp))
	return []byte(hex.EncodeToString(m.Sum(nil)))
}

// signSession returns a session cookie value valid until exp.
func (s *Server) signSession(exp time.Time) (string, error) {
	key, err := s.sessionKey(true)
	if err != nil {
		return "", err
	}
	v := strconv.FormatInt(exp.Unix(), 10)
	return v + "." + string(sessionMAC(key, v)), nil
}

// longSession reports whether r carries a valid, unexpired session cookie.
func (s *Server) longSession(r *http.Request, now time.Time) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	t, err := strconv.ParseInt(exp, 10, 64)
	if !ok || err != nil || now.Unix() >= t {
		return false
	}
	key, err := s.sessionKey(false) // read each time: a deleted key file revokes at once
	return err == nil && hmac.Equal([]byte(sig), sessionMAC(key, exp))
}

// pairLocal is POST /setup/pair on loopback under PairLoopback: the code gives a long-lived session.
func (s *Server) pairLocal(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.checkCode(w, r); !ok {
		return
	}
	v, err := s.signSession(time.Now().Add(sessionLife))
	if err != nil {
		s.Log.Error("setup: can't sign the session", "err", err)
		http.Error(w, "can't start the session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Cookies ignore ports: this one also goes to every other server on 127.0.0.1 (inherent to
	// loopback). Deleting the key file revokes every session.
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: v, Path: "/", MaxAge: int(sessionLife / time.Second),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	http.Redirect(w, r, "/setup/partdb", http.StatusSeeOther)
}
