package media

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// imageDelivery resizes large photos and picks the smallest format the viewer's browser supports.
// Videos keep their original URL: on-the-fly conversion can delay first playback and costs more quota.
const imageDelivery = "c_limit,w_1600,f_auto,q_auto"

// Cloudinary stores files through Cloudinary's upload API. Every request is signed on the server,
// so the secret never reaches a browser.
type Cloudinary struct {
	Cloud, Key, Secret, Base string
	Client                   *http.Client
}

func (c *Cloudinary) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{} // each call is limited by its context, which can be a long upload
}

func (c *Cloudinary) endpoint(resource, action string) string {
	return c.Base + "/v1_1/" + url.PathEscape(c.Cloud) + "/" + resource + "/" + action
}

// signRequest computes the signature Cloudinary expects: parameters sorted by name and joined
// with &, then the secret appended, then SHA-1. Empty values and the four parameters Cloudinary
// excludes aren't signed.
func signRequest(params map[string]string, secret string) string {
	names := make([]string, 0, len(params))
	for name, value := range params {
		switch name {
		case "file", "cloud_name", "resource_type", "api_key":
			continue
		}
		if value != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = name + "=" + params[name]
	}
	sum := sha1.Sum([]byte(strings.Join(parts, "&") + secret))
	return hex.EncodeToString(sum[:])
}

func (c *Cloudinary) signed(params map[string]string) map[string]string {
	params["timestamp"] = strconv.FormatInt(time.Now().Unix(), 10)
	params["signature"] = signRequest(params, c.Secret)
	params["api_key"] = c.Key
	return params
}

// Save implements Storage. Private files are stored as `authenticated`, so they can't be fetched
// without a signature. By this point a private file has already been encrypted.
func (c *Cloudinary) Save(ctx context.Context, f NewFile, r io.Reader) (Saved, error) {
	resource := f.Kind
	params := map[string]string{"public_id": "wearwhat/" + f.ID}
	if f.Private {
		// raw files keep their extension in the id, and are never processed or converted
		resource = "raw"
		params["public_id"] = "wearwhat/private/" + f.ID + f.Ext
		params["type"] = "authenticated"
	}
	params = c.signed(params)

	// stream the file, so a 50 MB video isn't held in memory
	body, form := io.Pipe()
	writer := multipart.NewWriter(form)
	go func() {
		form.CloseWithError(writeUploadForm(writer, params, f.ID+f.Ext, r))
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(resource, "upload"), body)
	if err != nil {
		body.Close()
		return Saved{}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := c.client().Do(req)
	if err != nil {
		return Saved{}, fmt.Errorf("cloudinary upload: %w", err)
	}
	defer res.Body.Close()

	var out struct {
		PublicID  string `json:"public_id"`
		SecureURL string `json:"secure_url"`
		Error     struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil && res.StatusCode < 300 {
		return Saved{}, fmt.Errorf("cloudinary upload: unreadable response: %w", err)
	}
	if res.StatusCode >= 300 {
		return Saved{}, fmt.Errorf("cloudinary upload: status %d: %s", res.StatusCode, out.Error.Message)
	}
	if out.PublicID == "" {
		return Saved{}, errors.New("cloudinary upload: no public_id in the response")
	}
	saved := Saved{Path: out.PublicID}
	if f.Private {
		return saved, nil
	}
	if !strings.HasPrefix(out.SecureURL, "https://") {
		return Saved{}, errors.New("cloudinary upload: no secure_url in the response")
	}
	saved.URL = out.SecureURL
	if f.Kind == "image" {
		saved.URL = strings.Replace(out.SecureURL, "/image/upload/", "/image/upload/"+imageDelivery+"/", 1)
	}
	return saved, nil
}

func writeUploadForm(w *multipart.Writer, fields map[string]string, filename string, r io.Reader) error {
	for name, value := range fields {
		if err := w.WriteField(name, value); err != nil {
			return err
		}
	}
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, r); err != nil {
		return err
	}
	return w.Close()
}

// Fetch implements Storage, using a signed link that expires after five minutes.
func (c *Cloudinary) Fetch(ctx context.Context, path string) ([]byte, error) {
	params := c.signed(map[string]string{
		"public_id":  path,
		"type":       "authenticated",
		"expires_at": strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10),
	})
	query := url.Values{}
	for name, value := range params {
		query.Set(name, value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("raw", "download")+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.client().Do(req)
	if err != nil {
		// the error from the HTTP client includes the signed link, so it's left out
		if ctx.Err() != nil {
			return nil, fmt.Errorf("cloudinary download: %w", ctx.Err())
		}
		return nil, errors.New("cloudinary download: couldn't reach Cloudinary")
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("cloudinary download: status %d: %s", res.StatusCode, res.Header.Get("X-Cld-Error"))
	}
	return io.ReadAll(io.LimitReader(res.Body, maxDocument+1<<20))
}
