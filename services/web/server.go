package web

import (
	"crypto/subtle"
	"encoding/json"
	"expvar"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/services/manage"
)

type Server struct {
	http.Server
	db db.Storage
}

type Config struct {
	// Hostname to use for download links
	Hostname string `toml:"hostname"`
	// Port is a server port to listen to
	Port int `toml:"port"`
	// Bind a specific IP addresses for server
	// "*": bind all IP addresses which is default option
	// localhost or 127.0.0.1  bind a single IPv4 address
	BindAddress string `toml:"bind_address"`
	// Flag indicating if the server will use TLS
	TLS bool `toml:"tls"`
	// Path to a certificate file for TLS connections
	CertificatePath string `toml:"certificate_path"`
	// Path to a private key file for TLS connections
	KeyFilePath string `toml:"key_file_path"`
	// Specify path for reverse proxy and only [A-Za-z0-9]
	Path string `toml:"path"`
	// DataDir is a path to a directory to keep XML feeds and downloaded episodes,
	// that will be available to user via web server for download.
	DataDir string `toml:"data_dir"`
	// WebUIEnabled is a flag indicating if web UI is enabled
	WebUIEnabled bool `toml:"web_ui"`
	// DebugEndpoints enables /debug/vars endpoint for runtime metrics (disabled by default)
	DebugEndpoints bool `toml:"debug_endpoints"`
	// NoIndex blocks search engine indexing by serving robots.txt and adding X-Robots-Tag header (disabled by default)
	NoIndex bool `toml:"no_index"`
	// NoListing returns 404 for directory listings, only serving actual files (disabled by default)
	NoListing bool `toml:"no_listing"`
	// ManagementUIEnabled enables the authenticated feed management page and API.
	ManagementUIEnabled bool `toml:"management_ui"`
	// ManagementToken authenticates management page and API requests.
	ManagementToken string `toml:"management_token"`
}

type FeedManager interface {
	List() ([]manage.Feed, error)
	Save(id string, input manage.Feed) (manage.Feed, error)
}

func New(cfg Config, storage http.FileSystem, database db.Storage, managers ...FeedManager) *Server {
	port := cfg.Port
	if port == 0 {
		port = 8080
	}

	bindAddress := cfg.BindAddress
	if bindAddress == "*" {
		bindAddress = ""
	}

	srv := Server{
		db: database,
	}

	srv.Addr = fmt.Sprintf("%s:%d", bindAddress, port)
	log.Debugf("using address: %s:%s", bindAddress, srv.Addr)

	// Use a custom mux instead of http.DefaultServeMux to avoid exposing
	// debug endpoints registered by imported packages (security fix for #799)
	mux := http.NewServeMux()

	fileServer := http.FileServer(storage)

	log.Debugf("handle path: /%s", cfg.Path)
	mux.Handle(fmt.Sprintf("/%s", cfg.Path), fileServer)

	// Add health check endpoint
	mux.HandleFunc("/health", srv.healthCheckHandler)

	if cfg.ManagementUIEnabled && len(managers) > 0 && managers[0] != nil {
		manager := managers[0]
		mux.HandleFunc("/manage", managementAuth(cfg.ManagementToken, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
				return
			}
			http.ServeFile(w, r, "./html/admin.html")
		}))
		mux.HandleFunc("/api/feeds", managementAuth(cfg.ManagementToken, feedCollectionHandler(manager)))
		mux.HandleFunc("/api/feeds/", managementAuth(cfg.ManagementToken, feedItemHandler(manager)))
	} else {
		mux.HandleFunc("/manage", http.NotFound)
		mux.HandleFunc("/api/feeds", http.NotFound)
		mux.HandleFunc("/api/feeds/", http.NotFound)
	}

	// Optionally enable debug endpoints (disabled by default for security)
	if cfg.DebugEndpoints {
		log.Info("debug endpoints enabled at /debug/vars")
		mux.Handle("/debug/vars", expvar.Handler())
	}

	srv.Handler = mux
	if cfg.NoIndex {
		log.Info("search engine indexing blocked (no_index enabled)")
		mux.HandleFunc("/robots.txt", robotsTxtHandler)
		srv.Handler = noIndexMiddleware(srv.Handler)
	}

	return &srv
}

func managementAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := r.URL.Query().Get("token")
		if provided == "" {
			provided = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func feedCollectionHandler(manager FeedManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		feeds, err := manager.List()
		if err != nil {
			writeManagementError(w, http.StatusInternalServerError, err)
			return
		}
		writeManagementJSON(w, http.StatusOK, map[string]interface{}{"feeds": feeds})
	}
}

func feedItemHandler(manager FeedManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/feeds/")
		if id == "" || strings.Contains(id, "/") {
			writeManagementError(w, http.StatusBadRequest, fmt.Errorf("invalid feed ID"))
			return
		}
		defer r.Body.Close()
		var input manage.Feed
		decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeManagementError(w, http.StatusBadRequest, fmt.Errorf("invalid request: %w", err))
			return
		}
		updated, err := manager.Save(id, input)
		if err != nil {
			writeManagementError(w, http.StatusBadRequest, err)
			return
		}
		writeManagementJSON(w, http.StatusOK, updated)
	}
}

func writeManagementJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.WithError(err).Error("failed to encode management response")
	}
}

func writeManagementError(w http.ResponseWriter, status int, err error) {
	writeManagementJSON(w, status, map[string]string{"error": err.Error()})
}

type HealthStatus struct {
	Status         string    `json:"status"`
	Timestamp      time.Time `json:"timestamp"`
	FailedEpisodes int       `json:"failed_episodes,omitempty"`
	Message        string    `json:"message,omitempty"`
}

func (s *Server) healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Check for recent download failures within the last 24 hours
	failedCount := 0
	cutoffTime := time.Now().Add(-24 * time.Hour)

	// Walk through all feeds to count recent failures
	err := s.db.WalkFeeds(ctx, func(feed *model.Feed) error {
		return s.db.WalkEpisodes(ctx, feed.ID, func(episode *model.Episode) error {
			if episode.Status == model.EpisodeError && episode.PubDate.After(cutoffTime) {
				failedCount++
			}
			return nil
		})
	})

	w.Header().Set("Content-Type", "application/json")

	status := HealthStatus{
		Timestamp: time.Now(),
	}

	if err != nil {
		log.WithError(err).Error("health check database error")
		status.Status = "unhealthy"
		status.Message = "database error during health check"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else if failedCount > 0 {
		status.Status = "unhealthy"
		status.FailedEpisodes = failedCount
		status.Message = fmt.Sprintf("found %d failed downloads in the last 24 hours", failedCount)
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		status.Status = "healthy"
		status.Message = "no recent download failures detected"
		w.WriteHeader(http.StatusOK)
	}

	json.NewEncoder(w).Encode(status)
}

func robotsTxtHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("User-agent: *\nDisallow: /\n"))
}

func noIndexMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}
