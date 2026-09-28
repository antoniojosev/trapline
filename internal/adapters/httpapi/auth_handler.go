package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/antoniojosev/trapline/internal/domain"
)

// sessionCookie is the cookie carrying the session token.
const sessionCookie = "trapline_session"

// adminContextKey is the type of the context key holding the authenticated
// admin. A named unexported type cannot collide with a key from another
// package, which a plain string could.
type adminContextKey struct{}

// adminFrom returns the authenticated admin from a request context. It is
// only ever populated by requireAuth for a cookie-authenticated request, so
// a handler behind that middleware can rely on it being present for the panel
// and absent for a token-authenticated caller.
func adminFrom(ctx context.Context) (domain.Admin, bool) {
	admin, ok := ctx.Value(adminContextKey{}).(domain.Admin)
	return admin, ok
}

// requireAuth accepts either credential and enforces the scope.
//
// Two credentials, one gate: the panel authenticates with a session cookie and
// the CLI, MCP or any integration with a bearer token. Both arrive at the same
// use cases, so neither can reach an operation the other cannot — which is
// what "one API, four clients" has to mean in practice (ADR 006).
//
// A session is the admin and carries every scope: it is the human who owns the
// installation. A token carries only what it was granted.
func (s *Server) requireAuth(required domain.Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, found := bearerToken(r); found {
			if _, err := s.tokens.Authenticate(r.Context(), token, required); err != nil {
				writeError(w, err)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			writeError(w, domain.ErrSessionNotFound)
			return
		}
		admin, err := s.auth.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			// Clear the cookie on the way out: leaving a dead one in the
			// browser means every later request pays a database lookup to
			// fail again.
			s.clearSessionCookie(w)
			writeError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminContextKey{}, admin)))
	})
}

// bearerToken reads an Authorization: Bearer header.
func bearerToken(r *http.Request) (token string, found bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type adminResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// handleSetupStatus reports whether the installation still needs its first
// admin, so the panel knows which screen to show before anyone is logged in.
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	needed, err := s.auth.NeedsSetup(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needs_setup": needed})
}

// handleSetup creates the first admin and logs them straight in: making
// someone type the password they just chose is friction with no security
// value.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var request setupRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	admin, err := s.auth.Setup(r.Context(), request.Username, request.Password)
	if err != nil {
		writeError(w, err)
		return
	}

	token, err := s.auth.Login(r.Context(), request.Username, request.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	s.setSessionCookie(w, token)
	writeJSON(w, http.StatusCreated, adminResponse{ID: admin.ID, Username: admin.Username})
}

// handleLogin opens a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var request setupRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	token, err := s.auth.Login(r.Context(), request.Username, request.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	s.setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLogout ends the session. It succeeds even with no cookie: signing out
// of an already dead session is not an error the user can act on.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := s.auth.Logout(r.Context(), cookie.Value); err != nil {
			writeError(w, err)
			return
		}
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMe returns the logged-in admin.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminFrom(r.Context())
	if !ok {
		writeError(w, errors.New("missing admin in an authenticated request"))
		return
	}
	writeJSON(w, http.StatusOK, adminResponse{ID: admin.ID, Username: admin.Username})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		// Secure follows the configured origin rather than being hard-coded:
		// forcing it on for an http:// origin would make the cookie be
		// dropped and login silently fail, which is a worse outcome than the
		// operator choosing plain HTTP knowingly.
		Secure:   s.origin.Scheme == "https",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(domain.SessionLifetime.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.origin.Scheme == "https",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
