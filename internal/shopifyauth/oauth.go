package shopifyauth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tanmaydikshit/vto-shopify-app/internal/config"
	"github.com/tanmaydikshit/vto-shopify-app/internal/store"
)

// shopDomainRe restricts the app param to "<name>.myshopify.com". This is an
// anti-SSRF / open-redirect guard: `shop` is attacker-controllable and we use
// it to build a URL we redirect to AND an endpoint we POST to. Never trust it
// raw.

var shopDomainRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*\.myshopify\.com$`)

const stateTTL = 10 * time.Minute

// oauthHTTPClient has a timeout so a hung shopify token endpoint can't wedge a
// request go-routine indefinitely (http.DefaultClient has no timeout).
var oauthHTTPClient = &http.Client{Timeout: 10 * time.Second}

// OAuth handler carries the config + store the install/callback flow needs.
// Its method are plain http.HandlerFunc values, so main.go mounts them on chi
// without this package importing the router.
type OAuthHandler struct {
	cfg   *config.Config
	store store.ShopStore
}

func NewOAuthHandler(cfg *config.Config, s store.ShopStore) *OAuthHandler {
	return &OAuthHandler{cfg: cfg, store: s}
}

// Install starts OAuth. GET /auth/install?shop=xxx.myshopify.com
func (h *OAuthHandler) Install(w http.ResponseWriter, r *http.Request) {
	shop := r.URL.Query().Get("shop")
	if !shopDomainRe.MatchString(shop) {
		http.Error(w, "invalid shop parameter", http.StatusBadRequest)
		return
	}

	state, err := randomState()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Persist the nonce, bound to this shop, with a short expiry. The callback
	// must present it back before we'll trust it.
	if err := h.store.SaveOAuthState(r.Context(), state, shop, time.Now().Add(stateTTL)); err != nil {
		log.Printf("oauth: save state : %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	authorizeURL := fmt.Sprintf(
		"https://%s/admin/oauth/authorize?client_id=%s&scope=%s&redirect_uri=%s&state=%s",
		shop,
		url.QueryEscape(h.cfg.ShopifyAPIKey),
		url.QueryEscape(h.cfg.ShopifyScopes),
		url.QueryEscape(h.cfg.AppURL+"/auth/callback"),
		url.QueryEscape(state),
	)
	http.Redirect(w, r, authorizeURL, http.StatusFound)
}

// Callback completes OAuth.
// GET /auth/callback?code=..&hmac=..&shop=..&state=..&timestamp=..
func (h *OAuthHandler) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	shop := q.Get("shop")
	if !shopDomainRe.MatchString(shop) {
		http.Error(w, "invalid shop parameter", http.StatusBadRequest)
		return
	}

	// (1) Authenticity: recompute the HMAC over the query and compare. Do this
	// FIRST - before trusting any other param.
	if !verifyOAuthHMAC(q, h.cfg.ShopifyAPISecret) {
		http.Error(w, "hmac verification failed", http.StatusBadRequest)
		return
	}

	// (2) CSRF + replay: the state must exist, be unexpired, and single-use, and
	// have been issued for THIS shop. Consume OAuthState deletes it atomically.
	stored, err := h.store.ConsumeOAuthState(r.Context(), q.Get("state"))
	if err != nil || stored != shop {
		http.Error(w, "invalid or expired state", http.StatusForbidden)
		return
	}

	// (3) Exchange the one-time code for a durable access token.
	tok, err := h.exchangeCode(r.Context(), shop, q.Get("code"))
	if err != nil {
		log.Printf("oauth: exchange code for %s : %v", shop, err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}

	// (4) Persist it - this is the whole point of the dance.
	if err := h.store.UpsertShop(r.Context(), store.Shop{
		Domain:      shop,
		AccessToken: tok.AccessToken,
		Scopes:      tok.Scope,
		InstalledAt: time.Now(),
	}); err != nil {
		log.Printf("oauth: upsert shop %s : %v", shop, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// (5) Land the merchant inside the embedded app in shopify admin.
	http.Redirect(w, r, fmt.Sprintf("https://%s/admin/apps/%s", shop, url.PathEscape(h.cfg.ShopifyAPIKey)), http.StatusFound)
}

// randomState returns 256 bits of crypto-random hex - unguessable, so an
// attacker  can't forge a valid state.
func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// verifyOAuthHMAC reproduces Shopiy's OAuth signature: every query param except
// `hmac`/`signature`, sorted by key, joined as key=value with '&', HMAC-SHA256
// keyed by the app secret, hex-encoded, compared in constant time.
func verifyOAuthHMAC(q url.Values, secret string) bool {
	given := q.Get("hmac")
	if given == "" {
		return false
	}

	keys := make([]string, 0, len(q))
	for k := range q {
		if k == "hmac" || k == "signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var msg strings.Builder
	for i, k := range keys {
		if i > 0 {
			msg.WriteByte('&')
		}
		// OAuth callback params are single-values; q.Get is correct here.
		msg.WriteString(k + "=" + q.Get(k))
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg.String()))
	expected := hex.EncodeToString(mac.Sum(nil))

	// hmac.Equal is constant-time - a plain == would leak timing info.
	return hmac.Equal([]byte(expected), []byte(given))
}

type accessTokenResp struct {
	AccessToken string `json:"access_token"`
	Scope       string `json:"scope"`
}

// exchangeCode POSTs the code to the shop's token endpoint and returns the token.
func (h *OAuthHandler) exchangeCode(ctx context.Context, shop, code string) (*accessTokenResp, error) {
	if code == "" {
		return nil, fmt.Errorf("missing code")
	}

	body, err := json.Marshal(map[string]string{
		"client_id":     h.cfg.ShopifyAPIKey,
		"client_secret": h.cfg.ShopifyAPISecret,
		"code":          code,
	})

	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("https://%s/admin/oauth/access_token", shop)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("shopify token endpoint returned %d", resp.StatusCode)
	}

	var out accessTokenResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	if out.AccessToken == "" {
		return nil, fmt.Errorf("empty access token in response")
	}

	return &out, nil
}
