package vto

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

type Item struct {
	ItemImageURL string `json:"item_image_url"`
	SubType      string `json:"sub_type"`
}

// Result is the rendered try-on outcome. It's a struct, not a bare string, so
// that if virtual-tryon turns out to be async we can add JobID/Status here
// without changing TryOn's signature. This is the sync/async seam.
type Result struct {
	ResultImageURL string `json:"result_image_url"`
	SubType        string `json:"sub_type"`
}

// CLient is the only component that talks to N8N, i.e. ai.talentool.in
type Client struct {
	uploadURL string
	tryonURL  string
	http      *http.Client
}

// NewClient wires two webhook endpoints. The timeout is deliberately
// generous: a syncronous render can take many seconds. Tune it once
// the workflow's real latency and sync-vs-async needs are clearer.
func NewClient(uploadURL, tryonURL string) *Client {
	return &Client{
		uploadURL: uploadURL,
		tryonURL:  tryonURL,
		http:      &http.Client{Timeout: 120 * time.Second},
	}
}

// -- upload ---------------------------------------------------------------

type uploadResp struct {
	// NOTE(integration): confirm the exact key the vto-user-upload webhook
	// returns. Assumed: {"user_image_url": "https://..."}
	UserImageURL string `json:"user_image_url"`
}

// Upload streams the shopper photo to the upload webhook as multipart form
// field "photo" and returns the S3 URL n8n stored at it.
func (c *Client) UploadPhoto(ctx context.Context, photo io.Reader, filename string) (string, error) {
	if filename == "" {
		filename = "photo.jpg"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("photo", filename)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}

	if _, err := io.Copy(part, photo); err != nil {
		return "", fmt.Errorf("copy photo: %w", err)
	}

	// MUST close before sending: this writes the closing multipart boundary.
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.uploadURL, &buf)
	if err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}
	// Take the Content-Type from the multipart writer.
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload webhook returned %s", resp.Status)
	}

	var out uploadResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}

	if out.UserImageURL == "" {
		return "", fmt.Errorf("upload response missing user_image_url")
	}

	return out.UserImageURL, nil

}

// -- try-on ---------------------------------------------------------------

type tryonReq struct {
	UserImageURL string `json:"user_image_url"`
	Items        []Item `json:"items"`
}

// TryOn asks the virtual try-on webhook to render items onto the shopper photo at
// userImageURL, and returns the rendered outcome result.
func (c *Client) TryOn(ctx context.Context, userImageURL string, items []Item) (Result, error) {
	body, err := json.Marshal(tryonReq{UserImageURL: userImageURL, Items: items})
	if err != nil {
		return Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tryonURL, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("tryon request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("tryon webhook returned %s", resp.Status)
	}

	var out Result
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("decode tryon response: %w", err)
	}

	if out.ResultImageURL == "" {
		return Result{}, fmt.Errorf("tryon response missing result_image_url")
	}

	return out, nil
}
