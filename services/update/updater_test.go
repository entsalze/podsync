package update

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mxpv/podsync/pkg/model"
)

func TestSelectEpisodesForDownloadHonoursRetentionWindow(t *testing.T) {
	now := time.Now()
	episodes := []*model.Episode{
		{ID: "old-new", PubDate: now.Add(-4 * time.Hour), Status: model.EpisodeNew},
		{ID: "newest", PubDate: now, Status: model.EpisodeNew},
		{ID: "downloaded", PubDate: now.Add(-time.Hour), Status: model.EpisodeDownloaded},
		{ID: "retry", PubDate: now.Add(-2 * time.Hour), Status: model.EpisodeError},
		{ID: "outside-window", PubDate: now.Add(-3 * time.Hour), Status: model.EpisodeNew},
	}

	actual := selectEpisodesForDownload(episodes, 3)

	assert.Equal(t, []string{"newest", "retry"}, episodeIDs(actual))
}

func TestSelectEpisodesForDownloadSortsByPublicationDate(t *testing.T) {
	now := time.Now()
	episodes := []*model.Episode{
		{ID: "middle", PubDate: now.Add(-time.Hour), Status: model.EpisodeNew},
		{ID: "oldest", PubDate: now.Add(-2 * time.Hour), Status: model.EpisodeNew},
		{ID: "newest", PubDate: now, Status: model.EpisodeNew},
	}

	actual := selectEpisodesForDownload(episodes, 2)

	assert.Equal(t, []string{"newest", "middle"}, episodeIDs(actual))
}

func TestSelectEpisodesForDownloadKeepsAllWhenUnlimited(t *testing.T) {
	now := time.Now()
	episodes := []*model.Episode{
		{ID: "new", PubDate: now, Status: model.EpisodeNew},
		{ID: "retry", PubDate: now.Add(-time.Hour), Status: model.EpisodeError},
		{ID: "downloaded", PubDate: now.Add(-2 * time.Hour), Status: model.EpisodeDownloaded},
	}

	actual := selectEpisodesForDownload(episodes, 0)

	assert.Equal(t, []string{"new", "retry"}, episodeIDs(actual))
}

func episodeIDs(episodes []*model.Episode) []string {
	ids := make([]string, 0, len(episodes))
	for _, episode := range episodes {
		ids = append(ids, episode.ID)
	}
	return ids
}
