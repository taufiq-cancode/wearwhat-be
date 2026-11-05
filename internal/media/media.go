// Package media stores uploaded files. Images and videos get public URLs; identity
// documents are private and have no HTTP route at all.
package media

import (
	"context"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"wearwhat-be/internal/app"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/secure"
)

const (
	maxImage    = 10 << 20
	maxDocument = 10 << 20
	maxVideo    = 50 << 20
)

// Storage is where file bytes live. Local disk today; an S3 implementation can replace it.
type Storage interface {
	Save(name string, r io.Reader) error
	Open(name string) (ReadSeekCloser, error)
}

type ReadSeekCloser interface {
	io.ReadSeeker
	io.Closer
}

// LocalDisk stores files under a directory.
type LocalDisk struct{ Dir string }

func (d LocalDisk) Save(name string, r io.Reader) error {
	full := filepath.Join(d.Dir, filepath.Clean(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func (d LocalDisk) Open(name string) (ReadSeekCloser, error) {
	return os.Open(filepath.Join(d.Dir, filepath.Clean(name)))
}

// allowed maps a sniffed content type to file extension and kind. No SVG: it can run script.
var allowed = map[string]struct {
	ext  string
	kind string
}{
	"image/jpeg":      {".jpg", "image"},
	"image/png":       {".png", "image"},
	"image/webp":      {".webp", "image"},
	"video/mp4":       {".mp4", "video"},
	"video/webm":      {".webm", "video"},
	"application/pdf": {".pdf", "document"},
}

type Service struct {
	*app.Deps
	Store Storage
}

// Upload is the response to a successful upload.
type Upload struct {
	ID     string  `json:"id"`
	URL    string  `json:"url"`
	Kind   string  `json:"kind"`
	Aspect float64 `json:"aspect,omitempty"`
}

// Routes registers the upload endpoint and the public file route.
func (s *Service) Routes(r *httpx.Router) {
	r.User("POST /uploads", s.upload)
	r.Mux.HandleFunc("GET /uploads/{id}", s.serve)
}

func (s *Service) upload(w http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	if err := httpx.Limit(r.Context(), s.DB, "upload:"+who.UserID, 120, time.Hour, "Too many uploads — try again later"); err != nil {
		return nil, err
	}
	purpose := r.URL.Query().Get("purpose") // image (default) | video | document

	r.Body = http.MaxBytesReader(w, r.Body, maxVideo+1<<20)
	file, _, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, httpx.BadRequest("That file is too large")
		}
		return nil, httpx.BadRequest("Choose a file to upload")
	}
	defer file.Close()
	if err := r.ParseMultipartForm(1 << 20); err == nil && purpose == "" {
		purpose = r.FormValue("purpose")
	}
	return s.store(r.Context(), who.UserID, purpose, file)
}

func (s *Service) store(ctx context.Context, userID, purpose string, file multipart.File) (*Upload, error) {
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n == 0 {
		return nil, httpx.BadRequest("That file is empty")
	}
	// the browser's claimed type is ignored: decide from the file's own bytes
	ctype := http.DetectContentType(head[:n])
	if strings.HasPrefix(ctype, "video/") == false && isMP4(head[:n]) {
		ctype = "video/mp4"
	}
	spec, ok := allowed[ctype]
	if !ok {
		return nil, httpx.BadRequest("That file type isn’t supported — use JPG, PNG or WebP")
	}

	isDocument := purpose == "document"
	limit := int64(maxImage)
	switch {
	case isDocument:
		// identity documents may be a PDF or a photo
		if spec.kind == "video" {
			return nil, httpx.BadRequest("Documents must be a PDF or a photo")
		}
		limit = maxDocument
	case spec.kind == "document":
		return nil, httpx.BadRequest("That file type isn’t supported — use JPG, PNG or WebP")
	case spec.kind == "video":
		limit = maxVideo
	}

	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size > limit {
		return nil, httpx.BadRequest("That file is too large")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var aspect *float64
	kind := spec.kind
	if isDocument {
		kind = "document"
	} else if spec.kind == "image" {
		cfg, _, err := image.DecodeConfig(file)
		if err != nil || cfg.Width == 0 || cfg.Height == 0 {
			return nil, httpx.BadRequest("That image couldn’t be read")
		}
		a := float64(cfg.Width) / float64(cfg.Height)
		aspect = &a
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}

	id := secure.ID("u")
	public := !isDocument
	name := id + spec.ext
	if !public {
		name = filepath.Join("private", name)
	}
	if err := s.Store.Save(name, file); err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO uploads (id, user_id, kind, content_type, size_bytes, path, is_public, aspect)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, userID, kind, ctype, size, name, public, aspect); err != nil {
		return nil, err
	}
	out := &Upload{ID: id, Kind: kind}
	if public {
		out.URL = "/uploads/" + id
	}
	if aspect != nil {
		out.Aspect = *aspect
	}
	return out, nil
}

func isMP4(head []byte) bool {
	return len(head) > 12 && string(head[4:8]) == "ftyp"
}

// serve streams a public file. Private documents answer 404, the same as a missing id.
func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	var path, ctype string
	var created time.Time
	err := s.DB.QueryRow(r.Context(), `SELECT path, content_type, created_at FROM uploads WHERE id = $1 AND is_public`,
		r.PathValue("id")).Scan(&path, &ctype, &created)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := s.Store.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(w, r, "", created, f)
}

var seededAsset = regexp.MustCompile(`^/assets/photos/[a-z0-9-]+\.jpg$`)

var uploadURL = regexp.MustCompile(`^/uploads/(u_[a-z2-7]+)$`)

// Photo turns a client-supplied URL into a Photo, allowing only our sample images and
// files uploaded by this person or their brand team. Foreign URLs are rejected.
func Photo(ctx context.Context, q db.Q, who *httpx.Principal, url string) (*domain.Photo, error) {
	url = strings.TrimSpace(url)
	if seededAsset.MatchString(url) {
		return &domain.Photo{URL: url}, nil
	}
	m := uploadURL.FindStringSubmatch(url)
	bad := httpx.BadRequest("That photo couldn’t be used — upload it again")
	if m == nil {
		return nil, bad
	}
	var kind string
	var aspect *float64
	err := q.QueryRow(ctx, `SELECT kind, aspect FROM uploads u WHERE u.id = $1 AND u.is_public AND (u.user_id = $2
		OR ($3 <> '' AND u.user_id IN (SELECT user_id FROM team_members WHERE brand_id = $3 AND status = 'active')))`,
		m[1], who.UserID, who.BrandID).Scan(&kind, &aspect)
	if db.IsNotFound(err) {
		return nil, bad
	}
	if err != nil {
		return nil, err
	}
	p := &domain.Photo{URL: url}
	if kind == "video" {
		p.Kind = "video"
	}
	if aspect != nil {
		p.Aspect = *aspect
	}
	return p, nil
}

// OptionalPhoto is Photo for fields that may be left empty.
func OptionalPhoto(ctx context.Context, q db.Q, who *httpx.Principal, url *string) (*domain.Photo, error) {
	if url == nil || strings.TrimSpace(*url) == "" {
		return nil, nil
	}
	return Photo(ctx, q, who, *url)
}

// OwnDocument checks a private upload belongs to this user and returns its id.
func OwnDocument(ctx context.Context, q db.Q, userID, id string) error {
	var exists bool
	err := q.QueryRow(ctx, `SELECT true FROM uploads WHERE id = $1 AND user_id = $2 AND NOT is_public`, id, userID).Scan(&exists)
	if db.IsNotFound(err) {
		return httpx.BadRequest("Upload your document again")
	}
	return err
}
