// Package client is a thin HTTP client for talking to a xetd server.
package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"xet-server/internal/api"
	"xet-server/internal/auth"
	"xet-server/internal/manifest"
)

// Client is a thin HTTP client for the Xet Data API (internal/api),
// mounted at api.V1 (sharing that namespace with, but never overlapping
// the specific paths of, the real CAS protocol). Cred, if set, is applied
// to every outgoing request via its FillCredential method (see
// auth.CredentialHelper) — nil is equivalent to
// auth.NoopCredentialHelper{}, this client's pre-v0.8.0 behavior of
// attaching no credential at all.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Cred    auth.CredentialHelper
}

func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: http.DefaultClient, Cred: auth.NoopCredentialHelper{}}
}

// route builds c.BaseURL+path, where path is one of internal/api's
// exported path constants (api.UploadPath, api.FilesPrefix+id,
// api.StatsPath, ...) — this client never re-types a version prefix or
// sub-path as its own string literal, so a path change on the server side
// only requires updating internal/api, not every client call site too.
func (c *Client) route(path string) string {
	return c.BaseURL + path
}

// do fills req's credential (if c.Cred is set) and sends it — every
// request this client makes should go through here rather than
// c.HTTP.Get/Post directly, so a configured CredentialHelper is never
// silently skipped on some code paths but not others.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.Cred != nil {
		if err := c.Cred.FillCredential(req); err != nil {
			return nil, fmt.Errorf("fill credential: %w", err)
		}
	}
	return c.HTTP.Do(req)
}

type UploadResult struct {
	FileID       string  `json:"file_id"`
	Name         string  `json:"name,omitempty"`
	Size         int64   `json:"size"`
	SHA256       string  `json:"sha256"`
	ChunksTotal  int     `json:"chunks_total"`
	ChunksNew    int     `json:"chunks_new"`
	BytesStored  int64   `json:"bytes_stored"`
	BytesOnWire  int64   `json:"bytes_on_wire"`
	DedupPercent float64 `json:"dedup_percent"`
}

// Push uploads the file at localPath and returns the server's summary,
// including how much of it deduplicated against chunks already stored.
func (c *Client) Push(localPath string) (*UploadResult, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	u := c.route(api.UploadPath) + "?name=" + url.QueryEscape(filenameOf(localPath))
	req, err := http.NewRequest(http.MethodPost, u, f)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upload failed: %s: %s", resp.Status, body)
	}
	var res UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Pull downloads fileID and writes it to localPath.
func (c *Client) Pull(fileID, localPath string) error {
	req, err := http.NewRequest(http.MethodGet, c.route(api.FilesPrefix+fileID), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download failed: %s: %s", resp.Status, body)
	}
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// Manifest fetches the reconstruction manifest for fileID.
func (c *Client) Manifest(fileID string) (*manifest.Manifest, error) {
	req, err := http.NewRequest(http.MethodGet, c.route(api.FilesPrefix+fileID+"/manifest"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("manifest fetch failed: %s: %s", resp.Status, body)
	}
	var m manifest.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Stats fetches store-wide dedup stats from the server.
func (c *Client) Stats() (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, c.route(api.StatsPath), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var stats map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, err
	}
	return stats, nil
}

func filenameOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}
