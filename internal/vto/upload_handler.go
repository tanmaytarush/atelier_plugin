package vto

import (
	"encoding/json"
	"log"
	"net/http"
)

// maxUploadBytes caps the shopper photo size. We reject anything larger BEFORE
// reading it into memory, so a hostile client can't OOM the process by streaming
// an endless body. 10 MiB comfortably covers a phone photo.
const maxUploadBytes = 10 << 20 // 10 MiB

// Handler is the HTTP boundary for the VTO request path. It owns the n8n client
// and nothing else - all VTO logic lives in n8n, this just translates HTTP <-> client.
// main.go constructs one of these as `vtoHandler` and mounts its methods under /apps/vto.
type Handler struct {
	client *Client
}

// NewHandler wires the n8n client into the HTTP layer.
func NewHandler(c *Client) *Handler {
	return &Handler{client: c}
}

// Upload handles POST /apps/vto/upload.
//
// Request:  multipart/form-data with a single file field "photo".
// Response: {"user_image_url": "https://...s3.../shopper.jpg"}
//
// The App Proxy HMAC check has already run as middleware by the time we get here,
// so we can trust this request came through a real Shopify storefront.
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	// Cap the body first. MaxBytesReader makes ParseMultipartForm fail cleanly
	// once the limit is exceeded instead of buffering an unbounded upload.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)

	// The memory arg is how much of the form is kept in RAM; the rest spills to
	// temp files. We only need the "photo" part, so a small figure is fine.
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid or oversized upload")
		return
	}

	file, header, err := r.FormFile("photo")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing photo field")
		return
	}
	defer file.Close()

	// Stream straight to n8n - no full copy into our own memory.
	userImageURL, err := h.client.UploadPhoto(r.Context(), file, header.Filename)
	if err != nil {
		// Log the real cause server-side; return an opaque 502 to the client so
		// we never leak n8n internals or the ai.talentool.in URL to the browser.
		log.Printf("vto upload: %v", err)
		writeError(w, http.StatusBadGateway, "upload failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"user_image_url": userImageURL})
}

// --- shared JSON helpers (also used by tryon_handler.go) -----------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Header/status already sent; nothing to do but log.
		log.Printf("vto: encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
