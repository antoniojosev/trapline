package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func newTokenRepo(t *testing.T) *TokenRepository {
	t.Helper()
	return NewTokenRepository(openTemp(t))
}

func mintToken(t *testing.T, name string, scopes []domain.Scope, expires *time.Time) (token domain.APIToken, plaintext string) {
	t.Helper()
	minted, plain, err := domain.MintAPIToken(name, scopes, testNow, expires)
	if err != nil {
		t.Fatalf("minting token: %v", err)
	}
	return minted, plain
}

func TestTokenRoundTrip(t *testing.T) {
	repo, ctx := newTokenRepo(t), context.Background()
	expiry := testNow.Add(24 * time.Hour)
	token, _ := mintToken(t, "cli", domain.AllScopes(), &expiry)

	if err := repo.Create(ctx, token); err != nil {
		t.Fatalf("creating: %v", err)
	}

	found, err := repo.FindValid(ctx, token.TokenHash, testNow)
	if err != nil {
		t.Fatalf("finding: %v", err)
	}
	if found.Name != "cli" {
		t.Errorf("Name = %q", found.Name)
	}
	if len(found.Scopes) != len(domain.AllScopes()) {
		t.Errorf("Scopes = %v, want all of them back", found.Scopes)
	}
	if !found.CreatedAt.Equal(testNow) {
		t.Errorf("CreatedAt = %v, want %v", found.CreatedAt, testNow)
	}
	if found.ExpiresAt == nil || !found.ExpiresAt.Equal(expiry) {
		t.Errorf("ExpiresAt = %v, want %v", found.ExpiresAt, expiry)
	}
	if found.LastUsed != nil {
		t.Error("a token that has never authenticated has a last-used time")
	}
}

func TestTokenWithoutExpiryRoundTrips(t *testing.T) {
	// A nil expiry has to survive storage as NULL, not as a far-future date
	// pretending to be forever.
	repo, ctx := newTokenRepo(t), context.Background()
	token, _ := mintToken(t, "forever", domain.AllScopes(), nil)

	if err := repo.Create(ctx, token); err != nil {
		t.Fatalf("creating: %v", err)
	}
	found, err := repo.FindValid(ctx, token.TokenHash, testNow.Add(100*365*24*time.Hour))
	if err != nil {
		t.Fatalf("a token with no expiry expired: %v", err)
	}
	if found.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil", found.ExpiresAt)
	}
}

func TestFindValidRejectsExpired(t *testing.T) {
	repo, ctx := newTokenRepo(t), context.Background()
	expiry := testNow.Add(time.Hour)
	token, _ := mintToken(t, "temporal", domain.AllScopes(), &expiry)

	if err := repo.Create(ctx, token); err != nil {
		t.Fatalf("creating: %v", err)
	}
	// Filtered in SQL, so an expired token is indistinguishable from an
	// unknown one and no path can load a stale row and then trust it.
	if _, err := repo.FindValid(ctx, token.TokenHash, expiry); !errors.Is(err, domain.ErrTokenNotFound) {
		t.Errorf("error = %v, want ErrTokenNotFound", err)
	}
}

func TestFindValidUnknown(t *testing.T) {
	repo, ctx := newTokenRepo(t), context.Background()
	if _, err := repo.FindValid(ctx, "no-such-hash", testNow); !errors.Is(err, domain.ErrTokenNotFound) {
		t.Errorf("error = %v, want ErrTokenNotFound", err)
	}
}

func TestListIsNewestFirst(t *testing.T) {
	repo, ctx := newTokenRepo(t), context.Background()

	older, _ := mintToken(t, "older", domain.AllScopes(), nil)
	newer, _, err := domain.MintAPIToken("newer", domain.AllScopes(), testNow.Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	for _, token := range []domain.APIToken{older, newer} {
		if err := repo.Create(ctx, token); err != nil {
			t.Fatalf("creating: %v", err)
		}
	}

	tokens, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("got %d tokens, want 2", len(tokens))
	}
	if tokens[0].Name != "newer" {
		t.Errorf("first listed = %q, want the newest", tokens[0].Name)
	}
}

func TestTouchLastUsed(t *testing.T) {
	repo, ctx := newTokenRepo(t), context.Background()
	token, _ := mintToken(t, "cli", domain.AllScopes(), nil)
	if err := repo.Create(ctx, token); err != nil {
		t.Fatalf("creating: %v", err)
	}

	used := testNow.Add(time.Minute)
	if err := repo.TouchLastUsed(ctx, token.TokenHash, used); err != nil {
		t.Fatalf("touching: %v", err)
	}

	found, err := repo.FindValid(ctx, token.TokenHash, used)
	if err != nil {
		t.Fatalf("finding: %v", err)
	}
	if found.LastUsed == nil || !found.LastUsed.Equal(used) {
		t.Errorf("LastUsed = %v, want %v", found.LastUsed, used)
	}
}

func TestDeleteTokenIsIdempotent(t *testing.T) {
	repo, ctx := newTokenRepo(t), context.Background()
	token, _ := mintToken(t, "cli", domain.AllScopes(), nil)
	if err := repo.Create(ctx, token); err != nil {
		t.Fatalf("creating: %v", err)
	}

	for attempt := range 3 {
		if err := repo.Delete(ctx, token.TokenHash); err != nil {
			t.Fatalf("delete attempt %d: %v", attempt+1, err)
		}
	}
	if _, err := repo.FindValid(ctx, token.TokenHash, testNow); !errors.Is(err, domain.ErrTokenNotFound) {
		t.Errorf("the token survived deletion: %v", err)
	}
}

func TestUnknownScopesInStorageDoNotBreakAToken(t *testing.T) {
	// A token written by a newer build must keep working; it simply is not
	// granted a scope this build cannot enforce.
	repo, ctx := newTokenRepo(t), context.Background()
	token, _ := mintToken(t, "from-the-future", domain.AllScopes(), nil)
	if err := repo.Create(ctx, token); err != nil {
		t.Fatalf("creating: %v", err)
	}
	if _, err := repo.db.ExecContext(ctx,
		"UPDATE api_tokens SET scopes = ? WHERE token_hash = ?",
		"projects:read,projects:teleport", token.TokenHash); err != nil {
		t.Fatalf("rewriting scopes: %v", err)
	}

	found, err := repo.FindValid(ctx, token.TokenHash, testNow)
	if err != nil {
		t.Fatalf("the token became unusable: %v", err)
	}
	if !found.Allows(domain.ScopeProjectsRead) {
		t.Error("the known scope was lost")
	}
	if found.Allows("projects:teleport") {
		t.Error("an unenforceable scope was granted")
	}
}
