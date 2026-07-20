package shopifyauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// VerifyAppProxy returns middleware that rejects any request whose Shopify
// App Proxy `signature` query param doesn't verify against the app secret.
// A valid signature proves the request was relayed by a real Shopify storefront,
// not crafted by a arbitrary client.

// The app proxy algorithm differs from the OAuth HMAC (see oauth.go):
// - the param is `signature`, not `hmac`
// - sorted "key=value" pairs are concatenated and hashed, with NO separator (OAuth uses '&')
// - a param with multiple values joins them with ','

func VerifyAppProxy(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// verify before the handler touches the body: an unauthenticated
			// request should be turned away without us reading/parsing anything.
			if !validAppProxySignature(r.URL.Query(), secret) {
				http.Error(w, "invalid app proxy signature", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func validAppProxySignature(q url.Values, secret string) bool {
	given := q.Get("signature")
	if given == "" {
		return false
	}

	// Collect every param except `signature`, sorted by key.
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == "signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build "k1=v1, k2=v2, ..." - pairs concatenated with no separator; multi-value
	// params join their values with a comma.
	var msg strings.Builder
	for _, k := range keys {
		msg.WriteString(k)
		msg.WriteByte('=')
		msg.WriteString(strings.Join(q[k], ","))
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg.String()))
	expected := hex.EncodeToString(mac.Sum(nil))

	// Constant time compare - same reasoning as the OAuth check
	return hmac.Equal([]byte(given), []byte(expected))
}
