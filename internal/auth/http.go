package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// maxBodyBytes caps request bodies. A username and password fit easily;
// anything bigger is a mistake or an attack.
const maxBodyBytes = 4 << 10

// Routes adds the account API to mux:
//
//	POST /api/register  {"username": "...", "password": "..."}  -> 201 Session
//	POST /api/login     {"username": "...", "password": "..."}  -> 200 Session
//	GET  /api/me        Authorization: Bearer <token>           -> 200 User
//
// The token is sent in the Authorization header, not in a cookie. Browsers
// never attach that header automatically, so another website can't make
// requests as you (the attack called CSRF).
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/register", s.handleRegister)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/me", s.handleMe)
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decodeJSON(w, r, &in) {
		return
	}
	session, err := s.Register(r.Context(), in.Username, in.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, session)
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decodeJSON(w, r, &in) {
		return
	}
	session, err := s.Login(r.Context(), in.Username, in.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	claims, err := s.tokens.Parse(BearerToken(r))
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeError(w, http.StatusUnauthorized, ErrInvalidToken.Error())
		return
	}
	writeJSON(w, http.StatusOK, claims.User())
}

// BearerToken returns the token from an "Authorization: Bearer <token>"
// header, or "" if there isn't one.
func BearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "request body must be a small JSON object")
		return false
	}
	return true
}

// writeAuthError turns a service error into the right HTTP status.
// Unexpected errors are logged, and the browser only sees a generic
// message, so database details never leak out.
func writeAuthError(w http.ResponseWriter, err error) {
	var invalid *ValidationError
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Error())
	case errors.Is(err, ErrUsernameTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, context.Canceled):
		// The browser went away before we finished. That's not a bug, so
		// don't log it, but still answer: an empty reply would be a 200.
		writeError(w, http.StatusServiceUnavailable, "request cancelled")
	default:
		slog.Error("auth request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store") // never cache tokens
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line has already gone out, so all we can do is note
		// it. This almost always means the browser hung up.
		slog.Debug("write JSON response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
