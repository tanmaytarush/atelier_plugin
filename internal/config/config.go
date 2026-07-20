package config

import (
	"fmt"
	"os"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Config holds process configuration loaded once at startup and then passed
// (read-only) into the handlers that need it.
type Config struct {
	// Shopify app credentials (from the Partners dashboard)
	ShopifyAPIKey    string `env:"SHOPIFY_API_KEY,required"`
	ShopifyAPISecret string `env:"SHOPIFY_API_SECRET,required"`
	ShopifyScopes    string `env:"SHOPIFY_SCOPES" envDefault:"read_products,write_products"`
	AppURL           string `env:"SHOPIFY_APP_URL,required"` // public https URL of this backend

	// Storage. For the dev-store phase this is a SQLite file path.
	DatabaseURL string `env:"DATABASE_URL" envDefault:"file:vto.db?_pragma=busy_timeout(5000)"`

	// n8n webhook endpoints — the only place ai.talentool.in is referred.
	VTOUploadWebhook string `env:"VTO_UPLOAD_WEBHOOK,required"`
	VTOTryonWebhook  string `env:"VTO_TRYON_WEBHOOK,required"`

	// HTTP listen port
	Port string `env:"PORT" envDefault:"8080"`
}

// Load reads configuration from the environment (and optional .env file).
func Load() (*Config, error) {
	// In production vars come from the host environment, so a missing .env
	// is not an error — we ignore that case only.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("loading .env: %w", err)
	}

	cfg, err := env.ParseAs[Config]()
	if err != nil {
		// caarlos0/env returns an aggregate error listing every failed field,
		// so a fresh setup surfaces all missing vars in one message.
		return nil, fmt.Errorf("parsing environment: %w", err)
	}

	return &cfg, nil
}
