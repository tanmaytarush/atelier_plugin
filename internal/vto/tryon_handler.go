package vto

import (
	"encoding/json"
	"log"
	"net/http"
)

// allowedSubTypes is the whitelist of garment/accessory categories the n8n
// pipeline knows how to render. sub_type comes from the storefront (a product
// metafield), so we must NOT trust it blindly - anything off this list is a
// client error, not something to forward to n8n.
var allowedSubTypes = map[string]bool{
	"dress":    true,
	"bracelet": true,
	"sandals":  true,
	"handbag":  true,
}

// tryonRequest is the flat body the storefront modal sends. It differs from the
// n8n client's shape (which nests an items[] array) - this handler is the
// translation layer between the two.
type tryonRequest struct {
	Shop            string `json:"shop"`
	ProductImageURL string `json:"product_image_url"`
	SubType         string `json:"sub_type"`
	UserImageURL    string `json:"user_image_url"`
}

// TryOn handles POST /apps/vto/tryon.
//
// Request:  {"shop","product_image_url","sub_type","user_image_url"}
// Response: {"result_image_url": "https://..."}
//
// App Proxy HMAC has already been verified by middleware before we reach here.
func (h *Handler) TryOn(w http.ResponseWriter, r *http.Request) {
	var req tryonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Validate every field is present. We accept sub_type + product_image_url
	// straight from the client (the "pass-through from Liquid" decision), so
	// field validation here is the only guard before we forward to n8n.
	if req.ProductImageURL == "" || req.SubType == "" || req.UserImageURL == "" {
		writeError(w, http.StatusBadRequest, "missing required field")
		return
	}

	if !allowedSubTypes[req.SubType] {
		writeError(w, http.StatusBadRequest, "unsupported sub_type")
		return
	}

	// Translate the flat storefront body into the client's items[] shape.
	result, err := h.client.TryOn(r.Context(), req.UserImageURL, []Item{
		{ItemImageURL: req.ProductImageURL, SubType: req.SubType},
	})
	if err != nil {
		// Opaque error to the client, real cause to the log - same rule as Upload.
		log.Printf("vto tryon (shop=%s sub_type=%s): %v", req.Shop, req.SubType, err)
		writeError(w, http.StatusBadGateway, "try-on failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"result_image_url": result.ResultImageURL})
}
