package builder

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"

	"github.com/mxpv/podsync/pkg/model"
)

type handleTransport struct {
	request *http.Request
}

func (transport *handleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.request = request
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"items": [{"id": "UCGvEFYE8RlYcz1XD26b_4Wg"}]
		}`)),
	}, nil
}

func TestListChannelsResolvesHandleExactly(t *testing.T) {
	transport := &handleTransport{}
	client := &http.Client{Transport: transport}
	service, err := youtube.NewService(context.Background(), option.WithHTTPClient(client))
	require.NoError(t, err)
	builder := &YouTubeBuilder{client: service, key: apiKey("test-api-key")}

	channel, err := builder.listChannels(context.Background(), model.TypeHandle, "MidnightASMR1", "id")
	require.NoError(t, err)
	require.Equal(t, "UCGvEFYE8RlYcz1XD26b_4Wg", channel.Id)
	require.NotNil(t, transport.request)
	require.Equal(t, "/youtube/v3/channels", transport.request.URL.Path)
	require.Equal(t, "MidnightASMR1", transport.request.URL.Query().Get("forHandle"))
	require.Empty(t, transport.request.URL.Query().Get("q"))
}

func TestParseURLWithHandles(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected model.Info
		wantErr  bool
	}{
		{
			name: "valid handle URL",
			url:  "https://www.youtube.com/@testhandle",
			expected: model.Info{
				LinkType: model.TypeHandle,
				Provider: model.ProviderYoutube,
				ItemID:   "testhandle",
			},
			wantErr: false,
		},
		{
			name: "handle URL with videos path",
			url:  "https://youtube.com/@mychannel/videos",
			expected: model.Info{
				LinkType: model.TypeHandle,
				Provider: model.ProviderYoutube,
				ItemID:   "mychannel",
			},
			wantErr: false,
		},
		{
			name:    "invalid handle URL",
			url:     "https://www.youtube.com/@",
			wantErr: true,
		},
		{
			name: "regular channel URL still works",
			url:  "https://www.youtube.com/channel/UC_test_channel",
			expected: model.Info{
				LinkType: model.TypeChannel,
				Provider: model.ProviderYoutube,
				ItemID:   "UC_test_channel",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseURL(tt.url)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expected.LinkType, result.LinkType)
			require.Equal(t, tt.expected.Provider, result.Provider)
			require.Equal(t, tt.expected.ItemID, result.ItemID)
		})
	}
}
