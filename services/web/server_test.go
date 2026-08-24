package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mxpv/podsync/pkg/fs"
	"github.com/mxpv/podsync/services/manage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockFileSystem struct{}

type mockFeedManager struct {
	feeds     []manage.Feed
	saved     manage.Feed
	deleted   string
	refreshed string
}

func (m *mockFeedManager) List() ([]manage.Feed, error) { return m.feeds, nil }
func (m *mockFeedManager) Save(id string, input manage.Feed) (manage.Feed, error) {
	input.ID = id
	m.saved = input
	return input, nil
}
func (m *mockFeedManager) Resolve(_ context.Context, _ string) (string, error) {
	return "Midnight_ASMR", nil
}
func (m *mockFeedManager) Delete(_ context.Context, id string) error {
	m.deleted = id
	return nil
}
func (m *mockFeedManager) Refresh(id string) error {
	m.refreshed = id
	return nil
}

func (m *mockFileSystem) Open(name string) (http.File, error) {
	return nil, http.ErrMissingFile
}

func TestManagementAPIDisabledByDefault(t *testing.T) {
	srv := New(Config{}, &mockFileSystem{}, nil, &mockFeedManager{})
	req := httptest.NewRequest(http.MethodGet, "/api/feeds?token=secret", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestManagementAPIRequiresToken(t *testing.T) {
	srv := New(Config{ManagementUIEnabled: true, ManagementToken: "secret"}, &mockFileSystem{}, nil, &mockFeedManager{})
	for _, path := range []string{"/api/feeds", "/api/feeds?token=wrong"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	}
}

func TestManagementPageIsRootAndLegacyManagePathIsGone(t *testing.T) {
	srv := New(Config{ManagementUIEnabled: true, ManagementToken: "secret"}, &mockFileSystem{}, nil, &mockFeedManager{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/manage?token=secret", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestManagementAPIListsAndSavesFeeds(t *testing.T) {
	manager := &mockFeedManager{feeds: []manage.Feed{{ID: "one", URL: "https://example.com", Enabled: true}}}
	srv := New(Config{ManagementUIEnabled: true, ManagementToken: "secret"}, &mockFileSystem{}, nil, manager)

	req := httptest.NewRequest(http.MethodGet, "/api/feeds?token=secret", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id":"one"`)

	body := strings.NewReader(`{"id":"one","url":"https://example.com","enabled":false,"media_type":"audio","audio_format":"m4a","audio_bitrate":"best","page_size":50,"keep_last":20,"minimum_duration":0,"update_period":"4h"}`)
	req = httptest.NewRequest(http.MethodPut, "/api/feeds/one?token=secret", body)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, manager.saved.Enabled)

	body = strings.NewReader(`{"url":"https://www.youtube.com/@MidnightASMR1"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/feeds/resolve?token=secret", body)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id":"Midnight_ASMR"`)

	req = httptest.NewRequest(http.MethodDelete, "/api/feeds/one?token=secret", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "one", manager.deleted)

	req = httptest.NewRequest(http.MethodPost, "/api/feeds/refresh/one?token=secret", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "one", manager.refreshed)
}

func TestDebugEndpointDisabledByDefault(t *testing.T) {
	cfg := Config{
		Port: 8080,
		Path: "feeds",
	}

	srv := New(cfg, &mockFileSystem{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	rec := httptest.NewRecorder()

	srv.Handler.ServeHTTP(rec, req)

	// Should return 404 when debug endpoints are disabled
	assert.Equal(t, http.StatusNotFound, rec.Code)
	// Should NOT contain expvar data
	assert.False(t, strings.Contains(rec.Body.String(), "cmdline"))
}

func TestDebugEndpointEnabledWhenConfigured(t *testing.T) {
	cfg := Config{
		Port:           8080,
		Path:           "feeds",
		DebugEndpoints: true,
	}

	srv := New(cfg, &mockFileSystem{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/debug/vars", nil)
	rec := httptest.NewRecorder()

	srv.Handler.ServeHTTP(rec, req)

	// Should return 200 and JSON content when debug endpoints are enabled
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	// Verify it contains expvar data (cmdline is always present)
	assert.True(t, strings.Contains(rec.Body.String(), "cmdline"))
}

func TestNoIndexDisabledByDefault(t *testing.T) {
	cfg := Config{
		Port: 8080,
		Path: "feeds",
	}

	srv := New(cfg, &mockFileSystem{}, nil)

	// robots.txt should return 404 when disabled
	req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// X-Robots-Tag header should not be present on feed requests
	req = httptest.NewRequest(http.MethodGet, "/feeds/test.xml", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Empty(t, rec.Header().Get("X-Robots-Tag"))
}

func TestNoIndexEnabledWhenConfigured(t *testing.T) {
	cfg := Config{
		Port:    8080,
		Path:    "feeds",
		NoIndex: true,
	}

	srv := New(cfg, &mockFileSystem{}, nil)

	// robots.txt should return disallow all
	req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/plain", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "User-agent: *")
	assert.Contains(t, rec.Body.String(), "Disallow: /")

	// X-Robots-Tag header should be present on all responses
	req = httptest.NewRequest(http.MethodGet, "/feeds/test.xml", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, "noindex, nofollow", rec.Header().Get("X-Robots-Tag"))
}

func TestNoListingDisabledByDefault(t *testing.T) {
	tmpDir := t.TempDir()

	// Create storage with NoListing disabled (default)
	storage, err := fs.NewLocal(tmpDir, false, false)
	require.NoError(t, err)

	// Create a file inside a subdirectory
	_, err = storage.Create(context.Background(), "feeds/episode.mp3", bytes.NewReader([]byte("audio content")))
	require.NoError(t, err)

	cfg := Config{
		Port: 8080,
		Path: "",
	}

	srv := New(cfg, storage, nil)

	// Accessing a directory should return 200 with directory listing
	req := httptest.NewRequest(http.MethodGet, "/feeds/", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "episode.mp3")

	// Accessing root should also return 200 with directory listing
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "feeds")

	// Accessing a file should work
	req = httptest.NewRequest(http.MethodGet, "/feeds/episode.mp3", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "audio content", rec.Body.String())
}

func TestNoListingEnabledWhenConfigured(t *testing.T) {
	tmpDir := t.TempDir()

	storage, err := fs.NewLocal(tmpDir, false, true)
	require.NoError(t, err)

	// Create a file inside a subdirectory
	_, err = storage.Create(context.Background(), "feeds/episode.mp3", bytes.NewReader([]byte("audio content")))
	require.NoError(t, err)

	cfg := Config{
		Port: 8080,
		Path: "",
	}

	srv := New(cfg, storage, nil)

	// Accessing a directory should return 404
	req := httptest.NewRequest(http.MethodGet, "/feeds/", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Accessing root should also return 404
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Accessing a file should still work
	req = httptest.NewRequest(http.MethodGet, "/feeds/episode.mp3", nil)
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "audio content", rec.Body.String())
}
