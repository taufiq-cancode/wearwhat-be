// Package media stores uploaded files. Images and videos get public URLs. Identity documents
// are encrypted first, and no HTTP route ever serves them.
package media

import (
	"bytes"
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
	"wearwhat-be/internal/config"
	"wearwhat-be/internal/db"
	"wearwhat-be/internal/domain"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/secure"
)

const (
	// 9 MB, not 10: Cloudinary's free plan stops at about 10 MB, and encryption adds a few bytes.
	maxImage    = 9 << 20
	maxDocument = 9 << 20
	maxVideo    = 50 << 20

	// a 50 MB video on mobile data, then forwarded to storage, outlasts the server's normal limits
	uploadTimeout = 10 * time.Minute
)

// NewFile describes a file about to be stored.
type NewFile struct {
	ID      string
	Ext     string // includes the dot
	Kind    string // image | video | document
	Private bool
}

// Saved says where a file ended up. URL is empty for private files.
type Saved struct {
	Path string
	URL  string
}

// Storage is where file bytes live: Cloudinary in production, local disk in development.
type Storage interface {
	Save(ctx context.Context, f NewFile, r io.Reader) (Saved, error)
	// Fetch returns a private file's stored bytes. It's for the admin tool, not for HTTP routes.
	Fetch(ctx context.Context, path string) ([]byte, error)
}

// NewStore picks Cloudinary when it's configured, and local disk otherwise.
func NewStore(cfg *config.Config) Storage {
	if cfg.CloudinaryEnabled() {
		return &Cloudinary{Cloud: cfg.CloudinaryCloud, Key: cfg.CloudinaryKey, Secret: cfg.CloudinarySecret, Base: cfg.CloudinaryBase}
	}
	return LocalDisk{Dir: cfg.UploadDir}
}

// LocalDisk stores files under a directory.
type LocalDisk struct{ Dir string }

// Save implements Storage.
func (d LocalDisk) Save(_ context.Context, f NewFile, r io.Reader) (Saved, error) {
	name := f.ID + f.Ext
	if f.Private {
		name = filepath.Join("private", name)
	}
	full := filepath.Join(d.Dir, filepath.Clean(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return Saved{}, err
	}
	out, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
	if err != nil {
		return Saved{}, err
	}
	defer out.Close()
	if _, err := io.Copy(out, r); err != nil {
		return Saved{}, err
	}
	saved := Saved{Path: name}
	if !f.Private {
		saved.URL = "/uploads/" + f.ID
	}
	return saved, nil
}

// Fetch implements Storage.
func (d LocalDisk) Fetch(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.Dir, filepath.Clean(path)))
}

func (d LocalDisk) open(name string) (*os.File, error) {
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

// Extension is the file extension for an upload's content type.
func Extension(contentType string) string { return allowed[contentType].ext }

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

// Routes registers the upload endpoint and, for files kept on local disk, the public file route.
// Cloudinary serves its own files, so it needs no route here.
func (s *Service) Routes(r *httpx.Router) {
	r.User("POST /uploads", s.upload)
	if local, ok := s.Store.(LocalDisk); ok {
		r.Mux.HandleFunc("GET /uploads/{id}", s.serve(local))
	}
}

func (s *Service) upload(w http.ResponseWriter, r *http.Request) (any, error) {
	who := httpx.Who(r)
	if err := httpx.Limit(r.Context(), s.DB, "upload:"+who.UserID, 120, time.Hour, "Too many uploads — try again later"); err != nil {
		return nil, err
	}
	purpose := r.URL.Query().Get("purpose") // image (default) | video | document

	deadline := time.Now().Add(uploadTimeout)
	rc := http.NewResponseController(w)
	if err := errors.Join(rc.SetReadDeadline(deadline), rc.SetWriteDeadline(deadline)); err != nil {
		s.Log.Warn("couldn't extend the upload deadline", "err", err)
	}

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
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	return s.store(ctx, who.UserID, purpose, file)
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
	target := NewFile{ID: id, Ext: spec.ext, Kind: kind, Private: isDocument}
	var body io.Reader = file
	if isDocument {
		sealed, err := s.seal(file)
		if err != nil {
			return nil, err
		}
		target.Ext = ".enc"
		body = bytes.NewReader(sealed)
	}
	saved, err := s.Store.Save(ctx, target, body)
	if err != nil {
		s.Log.Error("store upload", "kind", kind, "err", err)
		return nil, httpx.Err(http.StatusBadGateway, "That file couldn’t be saved — try again")
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO uploads (id, user_id, kind, content_type, size_bytes, path, url, is_public, aspect)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9)`, id, userID, kind, ctype, size, saved.Path, saved.URL, !isDocument, aspect); err != nil {
		return nil, err
	}
	out := &Upload{ID: id, Kind: kind, URL: saved.URL}
	if aspect != nil {
		out.Aspect = *aspect
	}
	return out, nil
}

func (s *Service) seal(file io.Reader) ([]byte, error) {
	plain, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return secure.EncryptBytes(s.Cfg.DataKey, plain)
}

// ReadDocument returns a private document's original bytes, for an admin to review.
func ReadDocument(ctx context.Context, store Storage, key []byte, path string) ([]byte, error) {
	sealed, err := store.Fetch(ctx, path)
	if err != nil {
		return nil, err
	}
	plain, err := secure.DecryptBytes(key, sealed)
	if err != nil {
		return nil, errors.New("couldn't decrypt this document: DATA_KEY isn't the one it was uploaded with")
	}
	return plain, nil
}

func isMP4(head []byte) bool {
	return len(head) > 12 && string(head[4:8]) == "ftyp"
}

// serve streams a public file from local disk. Private documents answer 404, the same as a
// missing id.
func (s *Service) serve(local LocalDisk) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var path, ctype string
		var created time.Time
		err := s.DB.QueryRow(r.Context(), `SELECT path, content_type, created_at FROM uploads WHERE id = $1 AND is_public`,
			r.PathValue("id")).Scan(&path, &ctype, &created)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f, err := local.open(path)
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
}

var seededAsset = regexp.MustCompile(`^/assets/photos/[a-z0-9-]+\.jpg$`)

// IsSample reports whether a URL is one of the bundled sample photos rather than an upload.
func IsSample(url string) bool { return seededAsset.MatchString(url) }

// Photo turns a client-supplied URL into a Photo, allowing only our sample images and files
// uploaded by this person or their brand team. The URL must match exactly what we stored, so
// foreign URLs, other people's files and altered Cloudinary transformations are all rejected.
func Photo(ctx context.Context, q db.Q, who *httpx.Principal, url string) (*domain.Photo, error) {
	url = strings.TrimSpace(url)
	if IsSample(url) {
		return &domain.Photo{URL: url}, nil
	}
	bad := httpx.BadRequest("That photo couldn’t be used — upload it again")
	if url == "" {
		return nil, bad
	}
	var kind string
	var aspect *float64
	err := q.QueryRow(ctx, `SELECT kind, aspect FROM uploads u WHERE u.url = $1 AND u.is_public AND (u.user_id = $2
		OR ($3 <> '' AND u.user_id IN (SELECT user_id FROM team_members WHERE brand_id = $3 AND status = 'active')))`,
		url, who.UserID, who.BrandID).Scan(&kind, &aspect)
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
