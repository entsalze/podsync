package manage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/pelletier/go-toml"
)

var feedIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type Feed struct {
	ID              string `json:"id"`
	URL             string `json:"url"`
	Enabled         bool   `json:"enabled"`
	MediaType       string `json:"media_type"`
	AudioFormat     string `json:"audio_format,omitempty"`
	AudioBitrate    string `json:"audio_bitrate,omitempty"`
	MaxHeight       int    `json:"max_height,omitempty"`
	PageSize        int    `json:"page_size"`
	KeepLast        int    `json:"keep_last"`
	MinimumDuration int64  `json:"minimum_duration"`
	UpdatePeriod    string `json:"update_period"`
	DiskUsage       int64  `json:"disk_usage"`
}

type Change struct {
	Feed      *feed.Config
	DeletedID string
}

type SourceResolver func(context.Context, string) (string, error)

type Store struct {
	path     string
	dataDir  string
	database db.Storage
	resolver SourceResolver
	onChange func(Change)
	mu       sync.Mutex
}

func NewStore(path, dataDir string, database db.Storage, resolver SourceResolver, onChange func(Change)) *Store {
	return &Store{path: path, dataDir: dataDir, database: database, resolver: resolver, onChange: onChange}
}

func (s *Store) List() ([]Feed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	configs, err := s.loadFeeds()
	if err != nil {
		return nil, err
	}
	result := make([]Feed, 0, len(configs))
	for id, config := range configs {
		item := toAPI(id, config)
		usage, err := s.diskUsage(id)
		if err != nil {
			return nil, err
		}
		item.DiskUsage = usage
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *Store) Save(id string, input Feed) (Feed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if input.ID != "" && input.ID != id {
		return Feed{}, fmt.Errorf("feed ID in request body does not match URL")
	}
	if err := validate(id, input); err != nil {
		return Feed{}, err
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		return Feed{}, fmt.Errorf("read config: %w", err)
	}
	var previous *feed.Config
	if current, currentErr := managedFeedFromDocument(data, id); currentErr == nil {
		previous = current
	}
	tree, err := toml.LoadBytes(data)
	if err != nil {
		return Feed{}, fmt.Errorf("parse config: %w", err)
	}
	feedsTree, _ := tree.Get("feeds").(*toml.Tree)
	if feedsTree == nil {
		feedsTree, _ = toml.TreeFromMap(map[string]interface{}{})
		tree.Set("feeds", feedsTree)
	}
	feedTree, _ := feedsTree.Get(id).(*toml.Tree)
	if feedTree == nil {
		feedTree, _ = toml.TreeFromMap(map[string]interface{}{})
		feedsTree.Set(id, feedTree)
	}

	config := fromAPI(id, input)
	setFeedTree(feedTree, config, input)
	encoded, err := encodeFeedTree(id, feedTree)
	if err != nil {
		return Feed{}, fmt.Errorf("encode config: %w", err)
	}
	merged, err := replaceFeedBlock(string(data), encoded, id)
	if err != nil {
		return Feed{}, err
	}
	config, err = managedFeedFromDocument([]byte(merged), id)
	if err != nil {
		return Feed{}, err
	}
	if err := writeConfig(s.path, []byte(merged), data); err != nil {
		return Feed{}, err
	}
	mediaTypeChanged := previous != nil && toAPI(id, previous).MediaType != input.MediaType
	var cleanupErr error
	if mediaTypeChanged {
		cleanupErr = s.deleteFeedData(context.Background(), id)
	}
	if s.onChange != nil {
		s.onChange(Change{Feed: config})
	}
	if cleanupErr != nil {
		return Feed{}, fmt.Errorf("feed format changed but old media cleanup was incomplete: %w", cleanupErr)
	}
	result := toAPI(id, config)
	result.DiskUsage, err = s.diskUsage(id)
	if err != nil {
		return Feed{}, err
	}
	return result, nil
}

func (s *Store) Resolve(ctx context.Context, sourceURL string) (string, error) {
	parsed, err := url.ParseRequestURI(sourceURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("source URL must be an absolute URL")
	}
	if s.resolver == nil {
		return "", fmt.Errorf("source name resolution is unavailable")
	}
	title, err := s.resolver(ctx, sourceURL)
	if err != nil {
		return "", fmt.Errorf("resolve source name: %w", err)
	}
	id := normaliseFeedID(title)
	if id == "" {
		return "", fmt.Errorf("source did not provide a usable channel name")
	}
	return id, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !feedIDPattern.MatchString(id) {
		return fmt.Errorf("invalid feed ID")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	configs, err := s.loadFeeds()
	if err != nil {
		return err
	}
	if _, ok := configs[id]; !ok {
		return fmt.Errorf("feed %q was not found", id)
	}
	if len(configs) == 1 {
		return fmt.Errorf("the final feed cannot be deleted because Podsync requires at least one feed")
	}

	updated, err := removeFeedBlock(string(data), id)
	if err != nil {
		return err
	}
	if err := writeConfig(s.path, []byte(updated), data); err != nil {
		return err
	}

	cleanupErr := s.deleteFeedData(ctx, id)
	if s.onChange != nil {
		s.onChange(Change{DeletedID: id})
	}
	if cleanupErr != nil {
		return fmt.Errorf("feed was removed from config but local cleanup was incomplete: %w", cleanupErr)
	}
	return nil
}

func (s *Store) deleteFeedData(ctx context.Context, id string) error {
	var cleanupErrors []error
	if s.database != nil {
		if err := s.database.DeleteFeed(ctx, id); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete database records: %w", err))
		}
	}
	if s.dataDir != "" {
		root, err := filepath.Abs(s.dataDir)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("resolve data directory: %w", err))
		} else {
			if err := os.RemoveAll(filepath.Join(root, id)); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("delete downloaded media: %w", err))
			}
			if err := os.Remove(filepath.Join(root, id+".xml")); err != nil && !os.IsNotExist(err) {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("delete feed XML: %w", err))
			}
		}
	}
	return errors.Join(cleanupErrors...)
}

func (s *Store) diskUsage(id string) (int64, error) {
	if s.dataDir == "" {
		return 0, nil
	}
	root, err := filepath.Abs(s.dataDir)
	if err != nil {
		return 0, fmt.Errorf("resolve data directory: %w", err)
	}
	var size int64
	err = filepath.Walk(filepath.Join(root, id), func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure media for feed %q: %w", id, err)
	}
	return size, nil
}

func normaliseFeedID(title string) string {
	id := strings.Join(strings.Fields(title), "_")
	id = regexp.MustCompile(`[^A-Za-z0-9_-]+`).ReplaceAllString(id, "_")
	return strings.Trim(id, "_")
}

func encodeFeedTree(id string, feedTree *toml.Tree) (string, error) {
	root, _ := toml.TreeFromMap(map[string]interface{}{})
	feeds, _ := toml.TreeFromMap(map[string]interface{}{})
	feeds.Set(id, feedTree)
	root.Set("feeds", feeds)
	return root.ToTomlString()
}

func replaceFeedBlock(original, encoded, id string) (string, error) {
	replacementLines := strings.Split(strings.ReplaceAll(encoded, "\r\n", "\n"), "\n")
	replacementStart, replacementEnd, found := feedBlockBounds(replacementLines, id, false)
	if !found {
		return "", fmt.Errorf("encoded feed block was not found")
	}
	replacement := replacementLines[replacementStart:replacementEnd]

	lineEnding := "\n"
	if strings.Contains(original, "\r\n") {
		lineEnding = "\r\n"
	}
	originalLines := strings.Split(strings.ReplaceAll(original, "\r\n", "\n"), "\n")
	start, end, exists := feedBlockBounds(originalLines, id, true)
	if exists {
		originalLines = append(append(originalLines[:start], replacement...), originalLines[end:]...)
	} else {
		for len(originalLines) > 0 && originalLines[len(originalLines)-1] == "" {
			originalLines = originalLines[:len(originalLines)-1]
		}
		originalLines = append(originalLines, "")
		originalLines = append(originalLines, replacement...)
		originalLines = append(originalLines, "")
	}
	return strings.Join(originalLines, lineEnding), nil
}

func removeFeedBlock(original, id string) (string, error) {
	lineEnding := "\n"
	if strings.Contains(original, "\r\n") {
		lineEnding = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(original, "\r\n", "\n"), "\n")
	start, end, found := feedBlockBounds(lines, id, true)
	if !found {
		return "", fmt.Errorf("feed %q was not found in config", id)
	}
	lines = append(lines[:start], lines[end:]...)
	for start > 0 && start < len(lines) && lines[start-1] == "" && lines[start] == "" {
		lines = append(lines[:start], lines[start+1:]...)
	}
	return strings.Join(lines, lineEnding), nil
}

func feedBlockBounds(lines []string, id string, preserveCommentedFeeds bool) (int, int, bool) {
	header := "[feeds." + id + "]"
	quotedHeader := "[feeds.\"" + id + "\"]"
	start := -1
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if start < 0 {
			if trimmed == header || trimmed == quotedHeader {
				start = index
			}
			continue
		}
		if preserveCommentedFeeds && isCommentedFeedHeader(trimmed) {
			return start, index, true
		}
		if strings.HasPrefix(trimmed, "[") && !isFeedSubtable(trimmed, id) {
			return start, index, true
		}
	}
	if start >= 0 {
		return start, len(lines), true
	}
	return 0, 0, false
}

func isCommentedFeedHeader(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#") {
		return false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
	return strings.HasPrefix(line, "[feeds.") || strings.HasPrefix(line, "[[feeds.")
}

func isFeedSubtable(line, id string) bool {
	return strings.HasPrefix(line, "[feeds."+id+".") ||
		strings.HasPrefix(line, "[[feeds."+id+".") ||
		strings.HasPrefix(line, "[feeds.\""+id+"\".") ||
		strings.HasPrefix(line, "[[feeds.\""+id+"\".")
}

func (s *Store) loadFeeds() (map[string]*feed.Config, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var document struct {
		Feeds   map[string]*feed.Config `toml:"feeds"`
		Cleanup *feed.Cleanup           `toml:"cleanup"`
	}
	if err := toml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	for id, config := range document.Feeds {
		applyDefaults(id, config, document.Cleanup)
	}
	return document.Feeds, nil
}

func validate(id string, input Feed) error {
	if !feedIDPattern.MatchString(id) {
		return fmt.Errorf("feed ID may contain only letters, numbers, underscores and hyphens")
	}
	parsed, err := url.ParseRequestURI(input.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("source URL must be an absolute URL")
	}
	if input.PageSize < 1 || input.PageSize > 1000 {
		return fmt.Errorf("items to inspect must be between 1 and 1000")
	}
	if input.KeepLast < 1 || input.KeepLast > 1000 {
		return fmt.Errorf("media to retain must be between 1 and 1000")
	}
	if input.MinimumDuration < 0 {
		return fmt.Errorf("minimum duration cannot be negative")
	}
	if _, err := time.ParseDuration(input.UpdatePeriod); err != nil {
		return fmt.Errorf("invalid update period: %w", err)
	}
	switch input.MediaType {
	case "audio":
		switch input.AudioFormat {
		case "m4a", "mp3", "opus":
		default:
			return fmt.Errorf("unsupported audio format")
		}
		if input.AudioBitrate != "best" {
			bitrate, err := strconv.Atoi(input.AudioBitrate)
			if err != nil || bitrate < 32 || bitrate > 320 {
				return fmt.Errorf("audio bitrate must be best or between 32 and 320 kbps")
			}
		}
	case "video":
		if input.MaxHeight < 0 {
			return fmt.Errorf("maximum height cannot be negative")
		}
	default:
		return fmt.Errorf("media type must be audio or video")
	}
	return nil
}

func fromAPI(id string, input Feed) *feed.Config {
	config := &feed.Config{
		ID: id, URL: input.URL, Disabled: !input.Enabled, PageSize: input.PageSize,
		UpdatePeriod: mustDuration(input.UpdatePeriod), Quality: model.QualityHigh,
		Filters: feed.Filters{MinDuration: input.MinimumDuration},
		Clean:   &feed.Cleanup{KeepLast: input.KeepLast}, OPML: true,
	}
	if input.MediaType == "video" {
		config.Format = model.FormatVideo
		config.MaxHeight = input.MaxHeight
		return config
	}
	config.AudioBitrate = input.AudioBitrate
	if input.AudioBitrate != "best" {
		config.AudioBitrate += "K"
	}
	switch input.AudioFormat {
	case "mp3":
		config.Format = model.FormatAudio
	case "opus":
		config.Format = model.FormatCustom
		config.CustomFormat = feed.CustomFormat{YouTubeDLFormat: "bestaudio[ext=webm][acodec^=opus]/bestaudio", Extension: "opus"}
	default:
		config.Format = model.FormatCustom
		config.CustomFormat = feed.CustomFormat{YouTubeDLFormat: "bestaudio[ext=m4a]/bestaudio[acodec^=mp4a]/bestaudio", Extension: "m4a"}
	}
	return config
}

func toAPI(id string, config *feed.Config) Feed {
	result := Feed{ID: id, URL: config.URL, Enabled: !config.Disabled, MediaType: "video",
		MaxHeight: config.MaxHeight, PageSize: config.PageSize,
		MinimumDuration: config.Filters.MinDuration, UpdatePeriod: config.UpdatePeriod.String()}
	if result.PageSize == 0 {
		result.PageSize = model.DefaultPageSize
	}
	if config.UpdatePeriod == 0 {
		result.UpdatePeriod = model.DefaultUpdatePeriod.String()
	}
	if config.Clean != nil {
		result.KeepLast = config.Clean.KeepLast
	}
	if config.Format == model.FormatAudio || config.Format == model.FormatCustom {
		result.MediaType = "audio"
		result.AudioFormat = "mp3"
		if config.Format == model.FormatCustom {
			extension := strings.ToLower(config.CustomFormat.Extension)
			if extension == "m4a" || extension == "opus" || extension == "mp3" {
				result.AudioFormat = extension
			}
		}
		result.AudioBitrate = strings.TrimSuffix(strings.ToUpper(config.AudioBitrate), "K")
		if result.AudioBitrate == "" || result.AudioBitrate == "BEST" {
			result.AudioBitrate = "best"
		}
	}
	return result
}

func setFeedTree(tree *toml.Tree, config *feed.Config, input Feed) {
	tree.Set("url", config.URL)
	tree.Set("disabled", config.Disabled)
	tree.Set("page_size", int64(config.PageSize))
	tree.Set("update_period", input.UpdatePeriod)
	_ = tree.Delete("cron_schedule")
	tree.Set("quality", string(config.Quality))
	tree.Set("format", string(config.Format))
	tree.Set("opml", true)
	clean, _ := tree.Get("clean").(*toml.Tree)
	if clean == nil {
		clean, _ = toml.TreeFromMap(map[string]interface{}{})
		tree.Set("clean", clean)
	}
	clean.Set("keep_last", int64(config.Clean.KeepLast))
	filters, _ := tree.Get("filters").(*toml.Tree)
	if filters == nil {
		filters, _ = toml.TreeFromMap(map[string]interface{}{})
		tree.Set("filters", filters)
	}
	filters.Set("min_duration", config.Filters.MinDuration)
	if input.MediaType == "video" {
		_ = tree.Delete("audio_bitrate")
		_ = tree.Delete("custom_format")
		tree.Set("max_height", int64(config.MaxHeight))
		return
	}
	_ = tree.Delete("max_height")
	tree.Set("audio_bitrate", config.AudioBitrate)
	if config.Format == model.FormatCustom {
		customFormat, _ := tree.Get("custom_format").(*toml.Tree)
		if customFormat == nil {
			customFormat, _ = toml.TreeFromMap(map[string]interface{}{})
			tree.Set("custom_format", customFormat)
		}
		customFormat.Set("youtube_dl_format", config.CustomFormat.YouTubeDLFormat)
		customFormat.Set("extension", config.CustomFormat.Extension)
	} else {
		_ = tree.Delete("custom_format")
	}
}

func managedFeedFromDocument(data []byte, id string) (*feed.Config, error) {
	var document struct {
		Feeds   map[string]*feed.Config `toml:"feeds"`
		Cleanup *feed.Cleanup           `toml:"cleanup"`
	}
	if err := toml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("validate updated config: %w", err)
	}
	if len(document.Feeds) == 0 {
		return nil, fmt.Errorf("updated config must contain at least one feed")
	}
	config, ok := document.Feeds[id]
	if !ok {
		return nil, fmt.Errorf("updated feed was not found in config")
	}
	applyDefaults(id, config, document.Cleanup)
	return config, nil
}

func applyDefaults(id string, config *feed.Config, cleanup *feed.Cleanup) {
	config.ID = id
	if config.PageSize == 0 {
		config.PageSize = model.DefaultPageSize
	}
	if config.UpdatePeriod == 0 {
		config.UpdatePeriod = model.DefaultUpdatePeriod
	}
	if config.Quality == "" {
		config.Quality = model.DefaultQuality
	}
	if config.Format == "" {
		config.Format = model.DefaultFormat
	}
	if config.Clean == nil && cleanup != nil {
		config.Clean = cleanup
	}
}

func writeConfig(path string, data, expected []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("re-read config: %w", err)
	}
	if !bytes.Equal(current, expected) {
		return fmt.Errorf("config changed while the feed was being edited; reload and try again")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("open config for writing: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	return nil
}

func mustDuration(value string) time.Duration {
	duration, _ := time.ParseDuration(value)
	return duration
}
