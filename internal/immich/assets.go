package immich

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MediaTypes lists the file extensions (with leading dot) the server accepts.
type MediaTypes struct {
	Image   []string `json:"image"`
	Video   []string `json:"video"`
	Sidecar []string `json:"sidecar"`
}

func (c *Client) MediaTypes(ctx context.Context) (MediaTypes, error) {
	var m MediaTypes
	err := c.do(ctx, http.MethodGet, "/server/media-types", nil, &m)
	return m, err
}

type CheckItem struct {
	ID       string `json:"id"`
	Checksum string `json:"checksum"` // SHA1, hex
}

type CheckResult struct {
	ID        string `json:"id"`
	Action    string `json:"action"` // accept | reject
	Reason    string `json:"reason"` // duplicate | unsupported-format
	AssetID   string `json:"assetId"`
	IsTrashed bool   `json:"isTrashed"`
}

// BulkUploadCheck asks which checksums the server already has.
func (c *Client) BulkUploadCheck(ctx context.Context, items []CheckItem) ([]CheckResult, error) {
	var r struct {
		Results []CheckResult `json:"results"`
	}
	err := c.do(ctx, http.MethodPost, "/assets/bulk-upload-check", map[string]any{"assets": items}, &r)
	return r.Results, err
}

type UploadResult struct {
	ID     string `json:"id"`
	Status string `json:"status"` // created | duplicate
}

// Upload streams one file to the server. The body is never held in memory.
func (c *Client) Upload(ctx context.Context, path, sha1hex string, modTime time.Time) (UploadResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return UploadResult{}, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		ts := modTime.UTC().Format("2006-01-02T15:04:05.000Z")
		err := func() error {
			for k, v := range map[string]string{
				"fileCreatedAt":  ts,
				"fileModifiedAt": ts,
				"filename":       filepath.Base(path),
			} {
				if err := mw.WriteField(k, v); err != nil {
					return err
				}
			}
			part, err := mw.CreateFormFile("assetData", filepath.Base(path))
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/assets", pr)
	if err != nil {
		pr.Close()
		return UploadResult{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-immich-checksum", sha1hex)
	if c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
	} else if c.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	}

	// Uploads can take a long time; rely on ctx instead of the client timeout.
	hc := *c.HTTP
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		pr.Close()
		return UploadResult{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Message any `json:"message"`
		}
		if json.Unmarshal(data, &e) == nil && e.Message != nil {
			msg = fmt.Sprint(e.Message)
		}
		return UploadResult{}, &APIError{Status: resp.StatusCode, Message: msg}
	}
	var r UploadResult
	return r, json.Unmarshal(data, &r)
}
