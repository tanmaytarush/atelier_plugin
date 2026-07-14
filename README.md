# Atelier

> Virtual try-on for Shopify — upload a photo, see the product on you.

A Theme App Extension drops a **Try It On** button on any product page. Shoppers open a modal, upload a photo, and get a rendered fit using the existing VTO pipeline at `ai.talentool.in`. Merchants keep their theme; shoppers never wait for a fitting room.

```
Product page  →  Try-On modal  →  Go backend  →  VTO webhooks  →  Result image
```

---

## Why this exists

Storefront AI try-on is powerful — and unsafe if the shopper’s browser can hit the model endpoints directly. Atelier puts a **Go app** between the storefront and the VTO pipeline:

- **App Proxy + HMAC** so only real Shopify traffic reaches upload / try-on
- **OAuth** so each merchant install is a first-class session
- **One place** that talks to n8n — credentials stay off the CDN and out of DevTools

Shopify CLI manages the theme extension and `shopify.app.toml`. The runtime is 100% Go.

---

## Architecture

```
┌─────────────────────────┐
│  Shopper (storefront)   │
│  Theme App Block        │
│  Liquid + vanilla JS    │
└───────────┬─────────────┘
            │  photo + product context
            ▼
┌─────────────────────────┐
│  Atelier (Go / chi)     │
│  • OAuth install        │
│  • App Proxy HMAC       │
│  • /apps/vto/upload     │
│  • /apps/vto/tryon      │
│  • Admin mapping UI     │
└───────────┬─────────────┘
            │  server-side only
            ▼
┌─────────────────────────┐
│  ai.talentool.in (n8n)  │
│  upload → S3            │
│  try-on → render        │
└─────────────────────────┘
```

**Rule of thumb:** storefront JS never calls `ai.talentool.in`. The Go binary is the only client.

---

## Stack

| Layer | Choice |
|---|---|
| Runtime | Go + [chi](https://github.com/go-chi/chi) |
| Shopify Admin / OAuth | [go-shopify](https://github.com/bold-commerce/go-shopify) |
| Shop sessions | Postgres / SQLite |
| Storefront | Theme App Extension (App Block) |
| Admin UI | Go `html/template` + App Bridge |
| Deploy tooling | Shopify CLI (config + extension only) |
| Hosting | Fly.io / Render / any Go host |

---

## Repo layout

```
cmd/server/          # process entrypoint
internal/
  config/            # env loading
  shopifyauth/       # OAuth, App Proxy HMAC, webhooks
  store/             # shop → access token
  vto/               # upload + try-on handlers, n8n client
  admin/             # merchant dashboard + subtype mapping
  middleware/        # rate limits, etc.
web/templates/       # embedded admin pages
extensions/          # Theme App Extension (button + modal)
shopify.app.toml
```

See `CLAUDE.md` for the full build plan, API contracts, and phase checklist.

---

## App Proxy API

Signed by Shopify on every request (`signature` query param — HMAC verified before handlers run).

### `POST /apps/vto/upload`

Multipart field `photo` → forwards to the upload webhook → S3 URL.

```json
{ "user_image_url": "https://.../shopper.jpg" }
```

### `POST /apps/vto/tryon`

```json
{
  "shop": "example.myshopify.com",
  "product_image_url": "https://cdn.shopify.com/.../product.jpg",
  "sub_type": "dress",
  "user_image_url": "https://.../shopper.jpg"
}
```

→ n8n virtual-try-on →

```json
{ "result_image_url": "https://..." }
```

Supported subtypes for Phase 1: `dress`, `bracelet`, `sandals`, `handbag`.

---

## Quick start

```bash
# Requirements: Go 1.22+, Shopify CLI (for theme extension deploy)

cp .env.example .env   # fill SHOPIFY_* and VTO_* URLs
go run ./cmd/server
```

Health check:

```bash
curl http://localhost:8080/healthz
# {"status":"ok"}
```

### Environment

```
SHOPIFY_API_KEY=
SHOPIFY_API_SECRET=
SHOPIFY_SCOPES=read_products,write_products
SHOPIFY_APP_URL=
DATABASE_URL=
VTO_UPLOAD_WEBHOOK=https://ai.talentool.in/webhook/vto-user-upload
VTO_TRYON_WEBHOOK=https://ai.talentool.in/webhook/virtual-tryon
PORT=8080
```

Dev store (current): `tryitout-dev.myshopify.com`

---

## Roadmap

| Phase | Focus |
|---|---|
| **1** | OAuth, App Proxy upload/try-on, modal on product pages, end-to-end on the dev store |
| **2** | Merchant subtype mapping UI, consent copy, theme-editor placement |
| **3** | Rate limits, photo retention, privacy policy, App Store readiness |

---

## Design principles

1. **Trust the signature, not the client** — App Proxy HMAC before any body work.
2. **Thin pass-through to VTO** — rendering stays in n8n; Go owns auth, routing, and abuse controls.
3. **Theme-safe** — Theme App Extension only; uninstall removes the storefront surface.
4. **Learnable** — this repo is built file-by-file; `CLAUDE.md` is the source of truth for how we work.

---

## Status

Early Phase 1 — chi server + health check is up. OAuth, App Proxy, and the storefront extension come next.

---

Built for merchants who want try-on without a theme rewrite. Built in Go so the pipeline stays under your control.
