# Virtual Try-On Shopify App — Development Plan (Go Backend)

**Goal:** Ship a Shopify app that adds a "Try It On" button to any merchant's product page. Clicking it opens a modal where a shopper uploads a photo and sees the product rendered on them, using the existing `ai.talentool.in` VTO pipeline.

**Dev store:** `tryitout-dev.myshopify.com`
**Existing backend (already working, do not rebuild):**
- `POST https://ai.talentool.in/webhook/vto-user-upload` — multipart form, field `photo` → returns S3 URL of uploaded shopper photo
- `POST https://ai.talentool.in/webhook/virtual-tryon` — JSON `{ user_image_url, items: [{ item_image_url, sub_type }] }` → returns rendered try-on result

**Backend language: Go.** Note — Shopify CLI (the tool used to scaffold/deploy the Theme App Extension and manage `shopify.app.toml`) is itself a Node-based CLI tool, but that's just a build/deploy utility; it does not run alongside your app or dictate your server's language. Your actual app server — OAuth, App Proxy routes, admin UI, Admin API calls — is 100% Go. There is no official Shopify Go template, so OAuth and App Proxy signature verification are implemented directly rather than via a scaffolding tool.

---

## 1. Architecture

```
Shopper's browser (storefront)
   │
   │  Theme App Extension "App Block" injected on product page
   │  (Liquid + JS/CSS, loads only where placed)
   ▼
Try-On Modal (vanilla JS / Web Component)
   │  1. upload photo
   │  2. request try-on
   ▼
Go Backend (single binary, net/http or chi router)
   │  - App Proxy routes: /apps/vto/upload, /apps/vto/tryon
   │  - HMAC-verifies every App Proxy request (query string signature)
   │  - OAuth install/callback routes for merchant onboarding
   │  - Admin API client (go-shopify) for product metafield reads/writes
   │  - Rate limiting per shop
   │  - Forwards to n8n webhooks — this is the ONLY place ai.talentool.in is called from
   ▼
n8n webhooks (ai.talentool.in) — UNCHANGED
   │  vto-user-upload → S3
   │  virtual-tryon → rendered image
   ▼
Response bubbles back up to modal
```

Never call `ai.talentool.in` directly from storefront JS — no auth today, so it's directly abusable from devtools. The Go backend is the only component with credentials to reach it.

---

## 2. Tech stack

| Layer | Choice | Why |
|---|---|---|
| App scaffold/deploy tool | Shopify CLI (`shopify app init`, `shopify app deploy`) | Only tool that manages `shopify.app.toml` and pushes the Theme App Extension — used purely for config/deploy, not as your runtime |
| Backend language | Go | Your requirement |
| HTTP router | `chi` (or stdlib `net/http` with middleware) | Lightweight, good for a small number of routes; avoids pulling in a heavy framework |
| Shopify Admin API client | `github.com/bold-commerce/go-shopify` | Maintained Go SDK covering REST Admin API + OAuth helper functions |
| Session/token storage | Postgres via `sqlx` or `pgx` (or SQLite for dev-store-only phase) | Store per-shop access token + install state |
| Storefront widget | Theme App Extension → App Block (Liquid + vanilla JS/CSS) | Only supported way to inject into product pages without editing merchant theme code; auto-removed on uninstall |
| Embedded admin UI | Server-rendered Go `html/template` pages + Shopify App Bridge CDN script | Avoids needing a separate JS frontend build for a small merchant dashboard (product → sub_type mapping) |
| Hosting | Fly.io / Render / a plain VPS with systemd | Any Go-friendly host; Fly.io is the least ceremony for a single Go binary |
| Photo storage | Existing S3 bucket via n8n | Add a lifecycle rule to auto-delete shopper photos after e.g. 24–72h |

---

## 3. Repo structure

```
vto-shopify-app/
├── cmd/
│   └── server/
│       └── main.go                # entrypoint, wires router + config
├── internal/
│   ├── config/
│   │   └── config.go               # env var loading
│   ├── shopifyauth/
│   │   ├── oauth.go                # install + callback handlers
│   │   ├── proxy_verify.go         # App Proxy HMAC signature check
│   │   └── webhook_verify.go       # webhook HMAC check (uninstall, GDPR topics)
│   ├── store/
│   │   └── shop_store.go           # Postgres/SQLite: shop → access token
│   ├── vto/
│   │   ├── upload_handler.go       # POST /apps/vto/upload → forwards to n8n
│   │   ├── tryon_handler.go        # POST /apps/vto/tryon → resolves sub_type, forwards to n8n
│   │   └── n8n_client.go           # thin HTTP client for the two n8n webhooks
│   ├── admin/
│   │   ├── dashboard_handler.go     # GET /admin → connection status
│   │   └── mapping_handler.go       # GET/POST /admin/mapping → product-to-sub_type UI
│   └── middleware/
│       └── ratelimit.go
├── web/
│   └── templates/
│       ├── dashboard.html.tmpl
│       └── mapping.html.tmpl
├── extensions/
│   └── vto-button/                 # Theme App Extension (managed by Shopify CLI, language-agnostic)
│       ├── blocks/
│       │   └── vto-button.liquid
│       ├── assets/
│       │   ├── vto-modal.js
│       │   └── vto-modal.css
│       └── shopify.extension.toml
├── shopify.app.toml
├── go.mod
├── go.sum
└── .env
```

---

## 4. Go implementation notes

**OAuth (merchant install):**
- `GET /auth/install?shop=xxx.myshopify.com` → build authorize URL (`client_id`, `scope`, `redirect_uri`, `state`) and redirect.
- `GET /auth/callback` → verify `hmac` query param against `SHOPIFY_API_SECRET`, exchange `code` for access token via `go-shopify`'s OAuth helper, persist `{shop, access_token}` in your store table.

**App Proxy signature verification (every `/apps/vto/*` request):**
Shopify signs App Proxy requests by appending a `signature` query param computed as `HMAC-SHA256(sorted, concatenated query params, app_secret)`. Implement this check in Go with `crypto/hmac` + `crypto/sha256` before processing any request — reject with 401 if it doesn't match. This is the mechanism that lets you trust a storefront request actually came through a real Shopify shop rather than a random client.

**Webhook verification:**
Standard Shopify webhooks (e.g. `app/uninstalled`) sign the raw body with `X-Shopify-Hmac-Sha256` header — verify the same way with the app secret before trusting payload.

**n8n client:**
A small wrapper with two methods, `UploadPhoto(io.Reader) (string, error)` and `TryOn(userImageURL string, items []Item) (Result, error)`, both just doing `http.Post` / `multipart.Writer` against the existing endpoints. This is a thin pass-through — all VTO logic stays in n8n.

**Sub_type resolution:**
Store `custom.vto_subtype` as a product metafield. Read it via `go-shopify`'s metafield API in `tryon_handler.go`, or — cheaper option — have the Liquid block pass `product.metafields.custom.vto_subtype` and the product image URL directly in the client request body, so the Go backend doesn't need an Admin API round trip on every try-on call (only needed for the merchant mapping UI writes).

---

## 5. API contracts (Go backend, called from the storefront modal)

**`POST /apps/vto/upload`** (App Proxy — HMAC-signed by Shopify)
```
Request:  multipart/form-data, field "photo"
Backend:  forwards to https://ai.talentool.in/webhook/vto-user-upload
Response: { "user_image_url": "https://...s3.../shopper.jpg" }
```

**`POST /apps/vto/tryon`**
```
Request:
{
  "shop": "tryitout-dev.myshopify.com",
  "product_image_url": "https://...cdn.shopify.com/.../product.jpg",
  "sub_type": "dress",
  "user_image_url": "https://...s3.../shopper.jpg"
}

Backend steps:
  1. Verify App Proxy signature
  2. Call https://ai.talentool.in/webhook/virtual-tryon with:
     { "user_image_url": ..., "items": [{ "item_image_url": product_image_url, "sub_type": sub_type }] }
  3. Return the rendered result to the client

Response: { "result_image_url": "https://..." }  (or job_id + poll route if n8n turns out to be async)
```

Confirm with the n8n workflow whether `virtual-tryon` is synchronous. If it's slow, add `GET /apps/vto/tryon/{jobId}` for polling and show a loading state client-side.

---

## 6. Build phases

**Phase 1 — Core loop on dev store**
1. `go mod init`, stand up `cmd/server/main.go` with chi router, health check route.
2. Implement OAuth install/callback against `tryitout-dev.myshopify.com`, confirm install completes and token is stored.
3. Scaffold the Theme App Extension via Shopify CLI: button + modal shell, place manually in theme editor on a product template.
4. Implement `/apps/vto/upload` and `/apps/vto/tryon` with hardcoded sub_type for 2-3 test products; validate full round trip against real n8n endpoints.
5. Wire modal JS: file input → upload → tryon call → render result image.
6. Manual test across dress, bracelet, sandals, handbag.

**Phase 2 — Merchant self-serve mapping**
1. Build `/admin/mapping` page (Go template + App Bridge): list products via Admin API, let merchant assign sub_type per product, write back as metafield.
2. Decide app-block placement UX: auto-inject on install vs. merchant adds via theme editor.
3. Add consent checkbox + privacy copy in the modal before upload is allowed.

**Phase 3 — Hardening for scale / App Store**
1. Per-shop rate limiting middleware on `/apps/vto/*`.
2. S3 lifecycle rule to expire shopper photos.
3. Performance pass: keep block asset size/load time within Shopify's Lighthouse-impact bar (<10% drop); Go backend responses <500ms for anything render-blocking.
4. Write privacy policy covering photo upload/retention (Shopify review + India's DPDP Act / GDPR if selling to EU shoppers).
5. Submit to Shopify App Store review.

---

## 7. Environment variables (Go backend)

```
SHOPIFY_API_KEY=
SHOPIFY_API_SECRET=
SHOPIFY_SCOPES=read_products,write_products   # write only needed if backend sets metafields itself
SHOPIFY_APP_URL=
DATABASE_URL=                                  # postgres or sqlite path
VTO_UPLOAD_WEBHOOK=https://ai.talentool.in/webhook/vto-user-upload
VTO_TRYON_WEBHOOK=https://ai.talentool.in/webhook/virtual-tryon
PORT=8080
```

---

## 8. Suggested Go dependencies

```
github.com/go-chi/chi/v5              # router
github.com/bold-commerce/go-shopify   # Admin API + OAuth helpers
github.com/jmoiron/sqlx               # or gorm.io/gorm, for shop/token storage
github.com/joho/godotenv              # local env loading
```

---

## 9. Open questions to resolve before/while building

- Is `virtual-tryon` synchronous or job-based? (determines whether you need a polling route)
- Any existing auth/API key on the n8n side, or is it fully open right now? If open, add a shared secret header before scaling past your own dev store.
- Photo retention policy — how long should shopper uploads live in S3?
- Auto-inject the app block on install, or have the merchant place it via theme editor (simpler, no extra scopes)?

---

## 10. Learning & documentation workflow (how to work in this repo)

This project is being built as a learning exercise. The person driving it wants to read and understand code, not watch it get written to disk automatically. **Plan mode is the default working state for this entire project** (set via `.claude/settings.json`), not just a pre-step before "real" work. Treat every session as read-only unless told otherwise.

**Never write files silently:**
- Do not use `Write`/`Edit`/`Bash` to create or change project files unless the person has explicitly said something like "write this file" or "apply this" for that specific piece of code. Producing a plan, a design, or a code sample is not permission to save it.
- Default output for any "build X" request is: explanation + the full code shown as a fenced code block in the response, not a file on disk. The person will copy it in themselves, or ask you to apply it once they're ready.

**One concept/file per turn:**
- Cover ONE file (or one tightly-related pair, e.g. a handler + its test) per turn. Don't dump a whole Phase at once.
- Before the code: explain what problem this file solves, what it depends on, what breaks without it.
- After the code: a short "what to notice" note — the Go idiom, the Shopify-specific gotcha, or the security reasoning behind a choice (e.g. why HMAC verification happens before touching the body).
- Stop after one file and wait — don't cascade into the next one automatically.

**Keep a decision log:**
- Maintain `docs/DECISIONS.md`. Whenever a non-trivial choice is made (library picked, auth flow shaped, a deviation from this plan), describe a dated 2-4 line entry to add — the person decides if/when it actually gets written.
- Maintain `docs/GLOSSARY.md` similarly for Shopify/Go-specific terms as they come up (App Proxy, metafield, HMAC, App Bridge, etc.) — plain-English definitions.

**Output style:**
- Turn on the Explanatory output style for this project (`/config` → Output style → Explanatory) so design-choice insights appear inline alongside the code, not just before/after it.

**If asked to actually implement:**
- Only exit plan mode for the specific file(s) explicitly approved, write those, then the person will return to plan mode (Shift+Tab) for the next explanation — don't treat one approval as license to keep writing.

**Verification, described not run:**
- For each piece of code, state in one line how it *could* be verified (a curl command, a `go test` invocation, a manual click-path) — don't assume it's correct, and don't run it against real files unless asked.

---

## 11. Verification checklist before calling Phase 1 done

- [ ] OAuth install/callback works end-to-end on the dev store, token persisted
- [ ] Button renders on product page without merchant editing Liquid
- [ ] App Proxy HMAC verification rejects unsigned/tampered requests (test with a manually crafted curl)
- [ ] Upload → S3 URL round trip works from the actual modal (not just curl)
- [ ] Try-on result renders correctly for all 4 known sub_types (dress, bracelet, sandals, handbag)
- [ ] No direct client-side calls to `ai.talentool.in` remain in shipped JS