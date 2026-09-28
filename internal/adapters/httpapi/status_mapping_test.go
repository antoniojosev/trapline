package httpapi

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

// TestEveryDomainErrorHasAStatus walks the domain's sentinel errors and fails
// on any that still map to 500.
//
// It exists because two of them did not, and both failures pointed the wrong
// way: asking for an issue that does not exist answered "our fault", and so
// did sending a malformed page cursor. A 500 is a page for an operator and a
// line in a log that reads like a bug — spending either on a client's typo is
// how real incidents get lost in the noise. The mapping is easy to forget when
// adding an error, so this asserts the whole set rather than the ones someone
// remembered.
func TestEveryDomainErrorHasAStatus(t *testing.T) {
	clientErrors := map[string]error{
		"invalid project":     domain.ErrInvalidProject,
		"invalid dsn":         domain.ErrInvalidDSN,
		"invalid origin":      domain.ErrInvalidOrigin,
		"invalid admin":       domain.ErrInvalidAdmin,
		"invalid token":       domain.ErrInvalidToken,
		"invalid issue":       domain.ErrInvalidIssue,
		"weak password":       domain.ErrWeakPassword,
		"project not found":   domain.ErrProjectNotFound,
		"key not found":       domain.ErrKeyNotFound,
		"admin not found":     domain.ErrAdminNotFound,
		"issue not found":     domain.ErrIssueNotFound,
		"token not found":     domain.ErrTokenNotFound,
		"session not found":   domain.ErrSessionNotFound,
		"invalid credentials": domain.ErrInvalidCredentials,
		"forbidden":           domain.ErrForbidden,
		"setup complete":      domain.ErrSetupComplete,
		"too many keys":       domain.ErrTooManyActiveKeys,
	}

	for name, sentinel := range clientErrors {
		t.Run(name, func(t *testing.T) {
			// Wrapped, because that is how they reach the adapter in practice.
			status, message := statusFor(fmt.Errorf("doing something: %w", sentinel))

			if status >= http.StatusInternalServerError {
				t.Errorf("%v maps to %d; a caller's mistake must not be reported as the server's", sentinel, status)
			}
			if status < http.StatusBadRequest {
				t.Errorf("%v maps to %d, which is not a failure at all", sentinel, status)
			}
			if message == "" {
				t.Errorf("%v maps to %d with no message", sentinel, status)
			}
		})
	}
}

func TestAnUnknownErrorIsAnInternalOne(t *testing.T) {
	// The default has to stay a 500: an error nobody classified is, by
	// definition, one we do not understand, and guessing 400 would blame the
	// caller for our own bug.
	status, message := statusFor(errUnclassified)
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
	if message != "internal error" {
		t.Errorf("message = %q; an unclassified error must not leak its detail", message)
	}
}

var errUnclassified = fmt.Errorf("something nobody thought about")
