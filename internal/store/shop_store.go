package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver; blank import used via database/sql
)

// ErrNotFound is returned when a lookup finds no matching row. Callers compare
// with errors.Is so they don't couple to sql.ErrNoRows (a storage-layer detail).
var ErrNotFound = errors.New("store: not found")

// Shop is one installed merchant. AccessToken is the offline Admin API token
// Shopify issues at the end of OAuth.
type Shop struct {
	Domain      string // e.g. "tryitout-dev.myshopify.com"
	AccessToken string
	Scopes      string // granted scopes, comma separated
	InstalledAt time.Time
}

// ShopStore is the persistence seam. Handlers depend on this interface; the
// concrete SQLiteStore below is swappable for a postgres one with no handler
// changes.
type ShopStore interface {
	UpsertShop(ctx context.Context, s Shop) error
	GetShop(ctx context.Context, domain string) (Shop, error)
	DeleteShop(ctx context.Context, domain string) error

	// OAuth CSRF nonce: save at /auth/install, consume (verify + delete) at /auth/callback.
	SaveOAuthState(ctx context.Context, state, shopDomain string, expiredAt time.Time) error
	ConsumeOAuthState(ctx context.Context, state string) (shopDomain string, err error)
}

// SQLiteStore implements ShopStore using a SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// compile-time assertion that SQLiteStore satisfies the interface.
var _ ShopStore = (*SQLiteStore)(nil)

// NewSQLiteStore opens the DB, enables WAL, and runs migrations.
func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite permits only one writer at a time. Capping the pool to a single
	// connection serializes access and sidesteps SQLITE_BUSY entirely - fine
	// for this low-traffic app. Postgres has no such limit; drop this there.
	db.SetMaxOpenConns(1)

	// WAL lets readers proceed during a write and is generally faster. It's a
	// persistent DB property, so setting it once here is fine.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	s := &SQLiteStore{db: db}
	if err := s.migrate(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) migrate(ctx context.Context) error {
	const schema = `
	CREATE TABLE IF NOT EXISTS shops (
		domain TEXT PRIMARY KEY,
		access_token TEXT NOT NULL,
		scopes TEXT NOT NULL,
		installed_at INTEGER NOT NULL -- unix seconds
	);

	CREATE TABLE IF NOT EXISTS oauth_states (
		state TEXT PRIMARY KEY,
		shop_domain TEXT NOT NULL,
		expires_at INTEGER NOT NULL -- unix seconds
	);`

	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// UpsertShop inserts or, if the shop re-installs, updates its token/scopes.
func (s *SQLiteStore) UpsertShop(ctx context.Context, sh Shop) error {
	const q = `
	INSERT INTO shops (domain, access_token, scopes, installed_at)
	VALUES (?, ?, ?, ?)
	ON CONFLICT(domain) DO UPDATE SET
		access_token = excluded.access_token,
		scopes = excluded.scopes,
		installed_at = excluded.installed_at;
	`

	_, err := s.db.ExecContext(ctx, q, sh.Domain, sh.AccessToken, sh.Scopes, sh.InstalledAt.Unix())
	if err != nil {
		return fmt.Errorf("upsert shop: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetShop(ctx context.Context, domain string) (Shop, error) {
	const q = `SELECT domain, access_token, scopes, installed_at FROM shops WHERE domain = ?;`
	var (
		sh        Shop
		installed int64
	)
	err := s.db.QueryRowContext(ctx, q, domain).
		Scan(&sh.Domain, &sh.AccessToken, &sh.Scopes, &installed)
	if errors.Is(err, sql.ErrNoRows) {
		return Shop{}, ErrNotFound
	}

	if err != nil {
		return Shop{}, fmt.Errorf("get shop: %w", err)
	}

	sh.InstalledAt = time.Unix(installed, 0)
	return sh, nil
}

func (s *SQLiteStore) DeleteShop(ctx context.Context, domain string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM shops WHERE domain = ?;", domain); err != nil {
		return fmt.Errorf("delete shop: %w", err)
	}
	return nil
}

func (s *SQLiteStore) SaveOAuthState(ctx context.Context, state, shopDomain string, expiresAt time.Time) error {
	const q = `INSERT INTO oauth_states (state, shop_domain, expires_at) VALUES (?, ?, ?);`
	if _, err := s.db.ExecContext(ctx, q, state, shopDomain, expiresAt.Unix()); err != nil {
		return fmt.Errorf("save oauth state: %w", err)
	}
	return nil
}

// ConsumeOAuthState atomically verifies (exists AND not expired) and deletes the
// state in one statement, so a nonce can never be replayed.
func (s *SQLiteStore) ConsumeOAuthState(ctx context.Context, state string) (string, error) {
	const q = `DELETE FROM oauth_states WHERE state = ? AND expires_at > ? RETURNING shop_domain;`
	var shopDomain string
	err := s.db.QueryRowContext(ctx, q, state, time.Now().Unix()).Scan(&shopDomain)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound // unknown, already consumed, or expired
	}
	if err != nil {
		return "", fmt.Errorf("consume oauth state: %w", err)
	}
	return shopDomain, nil
}

// Close releases th underlying DB handle. Call from main on shutdown.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
