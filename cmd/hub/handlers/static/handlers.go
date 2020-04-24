package static

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/artifacthub/hub/cmd/hub/handlers/helpers"
	"github.com/artifacthub/hub/internal/img"
	"github.com/go-chi/chi"
	svg "github.com/h2non/go-is-svg"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
)

const (
	indexCacheMaxAge  = 5 * time.Minute
	staticCacheMaxAge = 365 * 24 * time.Hour
)

// Handlers represents a group of http handlers in charge of handling
// static files operations.
type Handlers struct {
	cfg        *viper.Viper
	imageStore img.Store
	logger     zerolog.Logger
	indexBytes []byte

	mu          sync.RWMutex
	imagesCache map[string][]byte
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(cfg *viper.Viper, imageStore img.Store) *Handlers {
	h := &Handlers{
		cfg:         cfg,
		imageStore:  imageStore,
		imagesCache: make(map[string][]byte),
		logger:      log.With().Str("handlers", "static").Logger(),
	}
	h.prepareIndex()
	return h
}

// prepareIndex executes the index.html template and stores the resulting bytes
// that will be served by the ServeIndex handler.
func (h *Handlers) prepareIndex() {
	// Setup template
	path := path.Join(h.cfg.GetString("server.webBuildPath"), "index.html")
	text, err := ioutil.ReadFile(path)
	if err != nil {
		log.Panic().Err(err).Msg("error reading index.html template")
	}
	tmpl := template.Must(template.New("").Parse(string(text)))

	// Execute template
	var index bytes.Buffer
	data := map[string]string{
		"gaTrackingID": h.cfg.GetString("analytics.gaTrackingID"),
	}
	err = tmpl.Execute(&index, data)
	if err != nil {
		log.Panic().Err(err).Msg("error executing index.html template")
	}

	h.indexBytes = index.Bytes()
}

// Image is an http handler that serves images stored in the database.
func (h *Handlers) Image(w http.ResponseWriter, r *http.Request) {
	// Extract image id and version
	image := chi.URLParam(r, "image")
	parts := strings.Split(image, "@")
	var imageID, version string
	if len(parts) == 2 {
		imageID = parts[0]
		version = parts[1]
	} else {
		imageID = image
	}

	// Check if image version data is cached
	h.mu.RLock()
	data, ok := h.imagesCache[image]
	h.mu.RUnlock()
	if !ok {
		// Get image data from database
		var err error
		data, err = h.imageStore.GetImage(r.Context(), imageID, version)
		if err != nil {
			if errors.Is(err, img.ErrNotFound) {
				http.NotFound(w, r)
			} else {
				h.logger.Error().Err(err).Str("method", "Image").Str("imageID", imageID).Send()
				http.Error(w, "", http.StatusInternalServerError)
			}
			return
		}

		// Save image data in cache
		h.mu.Lock()
		h.imagesCache[image] = data
		h.mu.Unlock()
	}

	// Set headers and write image data to response writer
	w.Header().Set("Cache-Control", helpers.BuildCacheControlHeader(staticCacheMaxAge))
	if svg.Is(data) {
		w.Header().Set("Content-Type", "image/svg+xml")
	} else {
		w.Header().Set("Content-Type", http.DetectContentType(data))
	}
	_, _ = w.Write(data)
}

// SaveImage is an http handler that stores the provided image returning it id.
func (h *Handlers) SaveImage(w http.ResponseWriter, r *http.Request) {
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		h.logger.Error().Err(err).Str("method", "SaveImage").Msg("error reading body data")
		http.Error(w, "", http.StatusInternalServerError)
	}
	imageID, err := h.imageStore.SaveImage(r.Context(), data)
	if err != nil {
		h.logger.Error().Err(err).Str("method", "SaveImage").Send()
		http.Error(w, "", http.StatusInternalServerError)
	}
	dataJSON := []byte(fmt.Sprintf(`{"image_id": "%s"}`, imageID))
	helpers.RenderJSON(w, dataJSON, 0)
}

// ServeIndex is an http handler that serves the index.html file.
func (h *Handlers) ServeIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", helpers.BuildCacheControlHeader(indexCacheMaxAge))
	_, _ = w.Write(h.indexBytes)
}

// FileServer sets up a http.FileServer handler to serve static files from a
// a http.FileSystem.
func FileServer(r chi.Router, path string, fs http.FileSystem) {
	fsHandler := http.StripPrefix(path, http.FileServer(fs))

	if path != "/" && path[len(path)-1] != '/' {
		r.Get(path, http.RedirectHandler(path+"/", 301).ServeHTTP)
		path += "/"
	}
	path += "*"

	r.Get(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", helpers.BuildCacheControlHeader(staticCacheMaxAge))
		fsHandler.ServeHTTP(w, r)
	}))
}
