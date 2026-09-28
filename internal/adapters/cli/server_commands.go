package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/app"
	"github.com/antoniojosev/trapline/internal/config"
	"github.com/antoniojosev/trapline/internal/domain"
)

// runServe starts the server.
func runServe(c *context_, args []string) int {
	cfg, err := config.Load(args, c.env, c.stderr)
	if err != nil {
		if unwrapUsage(err) {
			return ExitUsage
		}
		return c.fail(err)
	}

	configureLogging(cfg.Debug, c.stderr)

	instance, err := app.New(c.ctx, cfg, Version)
	if err != nil {
		return c.fail(err)
	}
	defer func() {
		if err := instance.Close(); err != nil {
			slog.Error("closing database", "error", err)
		}
	}()

	if err := instance.Serve(c.ctx); err != nil {
		return c.fail(err)
	}
	return ExitOK
}

// configureLogging sends structured logs to stderr, keeping stdout clean for
// command output an agent may be parsing.
func configureLogging(debug bool, stderr any) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	writer, ok := stderr.(interface{ Write([]byte) (int, error) })
	if !ok {
		writer = os.Stderr
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: level})))
}

// openLocal opens the installation's database for the commands that operate on
// the file itself.
func openLocal(c *context_, dbPath string) (*app.App, error) {
	cfg, err := config.Load([]string{"-db", dbPath}, c.env, c.stderr)
	if err != nil {
		return nil, err
	}
	return app.New(c.ctx, cfg, Version)
}

// runBackup writes a consistent copy of the database.
func runBackup(c *context_, args []string) int {
	flags := newFlagSet(c, "backup")
	dbPath := flags.String("db", "", "path to the database (env TRAPLINE_DB)")
	to := flags.String("to", "", "destination file (required)")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *to == "" {
		fmt.Fprintln(c.stderr, "backup: -to is required")
		return ExitUsage
	}

	instance, err := openLocal(c, dbPathOf(c, *dbPath))
	if err != nil {
		return c.fail(err)
	}
	defer func() { _ = instance.Close() }()

	if err := instance.DB().Backup(c.ctx, *to); err != nil {
		return c.fail(err)
	}

	// The one thing a backup of this product does not contain, said every
	// time and not in a manual.
	//
	// Alert channel credentials are encrypted with a key that lives beside the
	// database and deliberately not inside it — a key stored in the thing it
	// encrypts protects nothing (ADR 015). The consequence lands months later,
	// on the worst day: the database is restored, the channels are all there,
	// and not one of them delivers, with nothing in the logs that points at a
	// missing file. So the file is named here, in the output of the command
	// whose whole job is "I have a copy of everything".
	payload := map[string]string{"status": "ok", "path": *to}
	text := "backup written to " + *to
	if keyPath, exists := secretKeyBeside(c, dbPathOf(c, *dbPath)); exists {
		payload["secret_key"] = keyPath
		payload["warning"] = "the secret key is NOT inside this backup; copy " + keyPath + " separately"
		text += "\n\nwarning: " + keyPath + " is NOT inside this backup.\n" +
			"         It is the key to your alert channel credentials, and without it a\n" +
			"         restored database lists its channels and delivers through none of them.\n" +
			"         Copy it separately, and keep it somewhere the backup is not."
	}
	return c.emit(*asJSON, payload, text)
}

// dbPathOf resolves the database path the same way the command that opened it
// did.
func dbPathOf(c *context_, flagValue string) string {
	return resolve(flagValue, c.env, "TRAPLINE_DB", config.DefaultDBPath)
}

// secretKeyBeside reports the key file for a database, and whether it exists.
//
// Whether: an installation that has never configured a channel has no key
// file, and warning it about a file it does not have would be teaching people
// to ignore the warning.
func secretKeyBeside(c *context_, dbPath string) (path string, exists bool) {
	path = keyFileFor(c, dbPath)
	if _, err := os.Stat(path); err != nil {
		return path, false
	}
	return path, true
}

// runRetention runs one sweep by hand.
//
// The server sweeps on its own, so this exists for the operator who wants the
// work to happen on their schedule instead of ours — a maintenance window, a
// cron entry, or simply reclaiming space now without waiting an hour.
func runRetention(c *context_, args []string) int {
	flags := newFlagSet(c, "retention")
	dbPath := flags.String("db", "", "path to the database (env TRAPLINE_DB)")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}

	instance, err := openLocal(c, resolve(*dbPath, c.env, "TRAPLINE_DB", config.DefaultDBPath))
	if err != nil {
		return c.fail(err)
	}
	defer func() { _ = instance.Close() }()

	result, err := instance.Retention().Sweep(c.ctx)
	if err != nil {
		return c.fail(err)
	}
	return c.emit(*asJSON,
		map[string]any{"deleted": result.Deleted, "projects": result.Projects},
		fmt.Sprintf("deleted %d events across %d projects", result.Deleted, result.Projects))
}

// runDownsample folds closed minute buckets into their hours by hand.
//
// Local, like retention and for the same reason: it is maintenance on the
// file, run on the box. The -age flag is what makes it useful beyond
// impatience — folding is only safe once nothing more can land in a minute,
// and how long that is depends on how an installation's SDKs batch. The
// default is the two hours the job uses; zero folds everything closed, which
// is what a gate that has just finished sending needs.
func runDownsample(c *context_, args []string) int {
	flags := newFlagSet(c, "downsample")
	dbPath := flags.String("db", "", "path to the database (env TRAPLINE_DB)")
	age := flags.Duration("age", domain.DownsampleAge,
		"only fold minutes that closed at least this long ago; 0 folds every closed minute")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *age < 0 {
		fmt.Fprintln(c.stderr, "downsample: -age cannot be negative")
		return ExitUsage
	}

	instance, err := openLocal(c, resolve(*dbPath, c.env, "TRAPLINE_DB", config.DefaultDBPath))
	if err != nil {
		return c.fail(err)
	}
	defer func() { _ = instance.Close() }()

	folded, err := instance.Downsample().FoldBefore(c.ctx, time.Now().UTC().Add(-*age))
	if err != nil {
		return c.fail(err)
	}
	return c.emit(*asJSON,
		map[string]any{"folded": folded},
		fmt.Sprintf("folded %d minute buckets into their hours", folded))
}

// runToken dispatches the token subcommands. They are local because a token is
// how you authenticate to the API in the first place: needing one to create
// one would be a bootstrap with no beginning.
func runToken(c *context_, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "token: expected create, list or revoke")
		return ExitUsage
	}
	switch args[0] {
	case "create":
		return runTokenCreate(c, args[1:])
	case "list":
		return runTokenList(c, args[1:])
	case "revoke":
		return runTokenRevoke(c, args[1:])
	default:
		fmt.Fprintf(c.stderr, "token: unknown subcommand %q\n", args[0])
		return ExitUsage
	}
}

type tokenOutput struct {
	Name      string    `json:"name"`
	Token     string    `json:"token,omitempty"`
	TokenHash string    `json:"token_hash"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt *string   `json:"expires_at"`
	LastUsed  *string   `json:"last_used"`
}

func runTokenCreate(c *context_, args []string) int {
	flags := newFlagSet(c, "token create")
	dbPath := flags.String("db", "", "path to the database (env TRAPLINE_DB)")
	name := flags.String("name", "", "what this token is for (required)")
	scopes := flags.String("scopes", "projects:read,projects:write", "comma-separated scopes")
	expiresIn := flags.Duration("expires-in", 0, "expiry as a duration, e.g. 720h; zero means never")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *name == "" {
		fmt.Fprintln(c.stderr, "token create: -name is required")
		return ExitUsage
	}

	requested := domain.DecodeScopes(*scopes)
	if len(requested) == 0 {
		fmt.Fprintf(c.stderr, "token create: no valid scopes in %q (known: %s)\n",
			*scopes, domain.EncodeScopes(domain.AllScopes()))
		return ExitUsage
	}

	instance, err := openLocal(c, resolve(*dbPath, c.env, "TRAPLINE_DB", config.DefaultDBPath))
	if err != nil {
		return c.fail(err)
	}
	defer func() { _ = instance.Close() }()

	var expiresAt *time.Time
	if *expiresIn > 0 {
		moment := time.Now().UTC().Add(*expiresIn)
		expiresAt = &moment
	}

	token, plaintext, err := instance.Tokens.Create(c.ctx, *name, requested, expiresAt)
	if err != nil {
		return c.fail(err)
	}

	output := newTokenOutput(token)
	// The plaintext appears exactly once, here. It is not recoverable
	// afterwards, which is the point: the token list must not be a place to
	// harvest credentials from.
	output.Token = plaintext

	return c.emit(*asJSON, output, plaintext+"\n\nThis token is shown once and cannot be recovered. Store it now.")
}

func runTokenList(c *context_, args []string) int {
	flags := newFlagSet(c, "token list")
	dbPath := flags.String("db", "", "path to the database (env TRAPLINE_DB)")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}

	instance, err := openLocal(c, resolve(*dbPath, c.env, "TRAPLINE_DB", config.DefaultDBPath))
	if err != nil {
		return c.fail(err)
	}
	defer func() { _ = instance.Close() }()

	tokens, err := instance.Tokens.List(c.ctx)
	if err != nil {
		return c.fail(err)
	}

	outputs := make([]tokenOutput, 0, len(tokens))
	var text strings.Builder
	for _, token := range tokens {
		outputs = append(outputs, newTokenOutput(token))
		lastUsed := "never used"
		if token.LastUsed != nil {
			lastUsed = "last used " + token.LastUsed.Format(time.RFC3339)
		}
		fmt.Fprintf(&text, "%s  %s  [%s]  %s\n",
			token.TokenHash[:12], token.Name, domain.EncodeScopes(token.Scopes), lastUsed)
	}
	if len(outputs) == 0 {
		return c.emit(*asJSON, outputs, "no tokens")
	}
	return c.emit(*asJSON, outputs, strings.TrimRight(text.String(), "\n"))
}

func runTokenRevoke(c *context_, args []string) int {
	flags := newFlagSet(c, "token revoke")
	dbPath := flags.String("db", "", "path to the database (env TRAPLINE_DB)")
	hash := flags.String("hash", "", "token hash as shown by `token list` (required)")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *hash == "" {
		fmt.Fprintln(c.stderr, "token revoke: -hash is required")
		return ExitUsage
	}

	instance, err := openLocal(c, resolve(*dbPath, c.env, "TRAPLINE_DB", config.DefaultDBPath))
	if err != nil {
		return c.fail(err)
	}
	defer func() { _ = instance.Close() }()

	// Revoking by full hash only. Accepting the truncated form the list shows
	// would be convenient and would eventually revoke the wrong credential on
	// a prefix collision, which is not a trade worth making for typing.
	if err := instance.Tokens.RevokeByHash(c.ctx, *hash); err != nil {
		return c.fail(err)
	}
	return c.emit(*asJSON, map[string]string{"status": "revoked"}, "revoked")
}

func newTokenOutput(token domain.APIToken) tokenOutput {
	output := tokenOutput{
		Name:      token.Name,
		TokenHash: token.TokenHash,
		Scopes:    make([]string, 0, len(token.Scopes)),
		CreatedAt: token.CreatedAt,
	}
	for _, scope := range token.Scopes {
		output.Scopes = append(output.Scopes, string(scope))
	}
	if token.ExpiresAt != nil {
		formatted := token.ExpiresAt.Format(time.RFC3339)
		output.ExpiresAt = &formatted
	}
	if token.LastUsed != nil {
		formatted := token.LastUsed.Format(time.RFC3339)
		output.LastUsed = &formatted
	}
	return output
}

// ensure context is referenced so the import stays meaningful if the file is
// edited down; runServe and the others all take one.
var _ = context.Background
