package manage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreSavePreservesUnmanagedSettingsAndNotifiesRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	initial := `# keep this comment
[server]
port = 8080

[cleanup]
keep_last = 12

[feeds.existing]
url = "https://www.youtube.com/channel/old"
page_size = 50
update_period = "6h"
format = "audio"
filename_template = "{{pub_date}}_{{id}}"

[feeds.existing.custom]
title = "Preserved title"

# [feeds.outdoor]
# url = "https://www.youtube.com/channel/disabled"
`
	require.NoError(t, os.WriteFile(path, []byte(initial), 0600))

	var change Change
	store := NewStore(path, "", nil, nil, func(value Change) { change = value })
	input := Feed{
		ID: "existing", URL: "https://www.youtube.com/channel/new", Enabled: false,
		MediaType: "audio", AudioFormat: "m4a", AudioBitrate: "192",
		PageSize: 100, KeepLast: 30, MinimumDuration: 600, UpdatePeriod: "4h",
	}
	saved, err := store.Save("existing", input)
	require.NoError(t, err)
	assert.False(t, saved.Enabled)
	assert.Equal(t, "m4a", saved.AudioFormat)
	assert.Equal(t, "192", saved.AudioBitrate)

	require.NotNil(t, change.Feed)
	assert.False(t, change.RunNow)
	assert.True(t, change.Feed.Disabled)
	assert.Equal(t, "{{pub_date}}_{{id}}", change.Feed.FilenameTemplate)
	assert.Equal(t, "Preserved title", change.Feed.Custom.Title)

	updated, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(updated), "# keep this comment")
	assert.Contains(t, string(updated), "# [feeds.outdoor]")
	assert.Contains(t, string(updated), `# url = "https://www.youtube.com/channel/disabled"`)
	assert.Contains(t, string(updated), `filename_template = "{{pub_date}}_{{id}}"`)
	assert.Contains(t, string(updated), `title = "Preserved title"`)
	assert.Contains(t, string(updated), "disabled = true")
}

func TestStoreNewFeedRunsImmediatelyAndManualRefreshQueuesExistingFeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[feeds.existing]\nurl = \"https://example.com/existing\"\n"), 0600))

	var changes []Change
	store := NewStore(path, "", nil, nil, func(value Change) { changes = append(changes, value) })
	_, err := store.Save("new_feed", Feed{
		ID: "new_feed", URL: "https://example.com/new", Enabled: true,
		MediaType: "audio", AudioFormat: "m4a", AudioBitrate: "best",
		PageSize: 100, KeepLast: 20, MinimumDuration: 600, UpdatePeriod: "4h",
	})
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.True(t, changes[0].RunNow)
	assert.Equal(t, "new_feed", changes[0].Feed.ID)

	require.NoError(t, store.Refresh("existing"))
	require.Len(t, changes, 2)
	assert.Equal(t, "existing", changes[1].RefreshID)
}

func TestStoreListAppliesDefaultsAndGlobalCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[cleanup]
keep_last = 15
[feeds.one]
url = "https://example.com/feed"
format = "audio"
`), 0600))

	feeds, err := NewStore(path, "", nil, nil, nil).List()
	require.NoError(t, err)
	require.Len(t, feeds, 1)
	assert.Equal(t, 50, feeds[0].PageSize)
	assert.Equal(t, 15, feeds[0].KeepLast)
	assert.Equal(t, "6h0m0s", feeds[0].UpdatePeriod)
	assert.Equal(t, "mp3", feeds[0].AudioFormat)
}

func TestStoreRejectsInvalidFeedIDWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	initial := "[feeds.one]\nurl = \"https://example.com/feed\"\n"
	require.NoError(t, os.WriteFile(path, []byte(initial), 0600))

	_, err := NewStore(path, "", nil, nil, nil).Save("../bad", Feed{})
	require.Error(t, err)
	updated, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, initial, string(updated))
}

func TestStoreResolveUsesSourceTitleForFeedID(t *testing.T) {
	resolver := func(_ context.Context, _ string) (string, error) { return "Midnight ASMR", nil }
	store := NewStore("unused", "", nil, resolver, nil)

	id, err := store.Resolve(context.Background(), "https://www.youtube.com/@MidnightASMR1")
	require.NoError(t, err)
	assert.Equal(t, "Midnight_ASMR", id)
}

func TestStoreDeleteRemovesConfigAndLocalFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	initial := "[feeds.one]\nurl = \"https://example.com/one\"\n\n[feeds.two]\nurl = \"https://example.com/two\"\n"
	require.NoError(t, os.WriteFile(path, []byte(initial), 0600))
	require.NoError(t, os.Mkdir(filepath.Join(root, "one"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "one", "episode.m4a"), []byte("audio"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "one.xml"), []byte("rss"), 0600))

	var change Change
	store := NewStore(path, root, nil, nil, func(value Change) { change = value })
	require.NoError(t, store.Delete(context.Background(), "one"))
	assert.Equal(t, "one", change.DeletedID)

	updated, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(updated), "[feeds.one]")
	assert.Contains(t, string(updated), "[feeds.two]")
	_, err = os.Stat(filepath.Join(root, "one"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(root, "one.xml"))
	assert.True(t, os.IsNotExist(err))
}

func TestStoreMediaTypeChangeRemovesOldMediaAndReportsUsage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	initial := "[feeds.one]\nurl = \"https://example.com/one\"\nformat = \"audio\"\npage_size = 50\nupdate_period = \"4h\"\nclean = { keep_last = 20 }\n\n[feeds.two]\nurl = \"https://example.com/two\"\n"
	require.NoError(t, os.WriteFile(path, []byte(initial), 0600))
	require.NoError(t, os.Mkdir(filepath.Join(root, "one"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "one", "episode.m4a"), []byte("audio"), 0600))

	store := NewStore(path, root, nil, nil, nil)
	feeds, err := store.List()
	require.NoError(t, err)
	require.Len(t, feeds, 2)
	assert.Equal(t, int64(5), feeds[0].DiskUsage)

	_, err = store.Save("one", Feed{
		ID: "one", URL: "https://example.com/one", Enabled: true,
		MediaType: "video", MaxHeight: 1080, PageSize: 50, KeepLast: 20, UpdatePeriod: "4h",
	})
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "one"))
	assert.True(t, os.IsNotExist(err))
}
