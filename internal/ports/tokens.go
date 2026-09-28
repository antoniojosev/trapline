package ports

import (
	"context"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// TokenRepository stores API tokens.
type TokenRepository interface {
	Create(ctx context.Context, token domain.APIToken) error
	// FindValid resolves a token hash, rejecting expired ones. Returns
	// domain.ErrTokenNotFound when unknown or expired.
	FindValid(ctx context.Context, tokenHash string, now time.Time) (domain.APIToken, error)
	List(ctx context.Context) ([]domain.APIToken, error)
	Delete(ctx context.Context, tokenHash string) error
	// TouchLastUsed records that a token authenticated a request. Best
	// effort: a failure here must never fail the request it is describing,
	// so callers ignore its error deliberately.
	TouchLastUsed(ctx context.Context, tokenHash string, now time.Time) error
}
