package stripchat

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/HeapOfChaos/goondvr/internal"
	"github.com/HeapOfChaos/goondvr/site"
)

// Stripchat implements site.Site for the Stripchat platform.
type Stripchat struct{}

// New returns a new Stripchat site implementation.
func New() *Stripchat {
	return &Stripchat{}
}

// camResponse is the relevant subset of the Stripchat cam API response.
type camResponse struct {
	Cam struct {
		StreamName        string            `json:"streamName"`
		IsCamActive       bool              `json:"isCamActive"`
		ViewServers       map[string]string `json:"viewServers"`
		BroadcastSettings struct {
			BroadcastType string `json:"broadcastType"`
		} `json:"broadcastSettings"`
		Topic string `json:"topic"`
	} `json:"cam"`
	User struct {
		User struct {
			ID                 int64  `json:"id"`
			Username           string `json:"username"`
			IsOnline           bool   `json:"isOnline"`
			Status             string `json:"status"`
			BroadcastGender    string `json:"broadcastGender"`
			PreviewUrlThumbBig string `json:"previewUrlThumbBig"`
			BroadcastServer    string `json:"broadcastServer"`
			SnapshotTimestamp  int64  `json:"snapshotTimestamp"`
		} `json:"user"`
	} `json:"user"`
}

// mapGender converts Stripchat gender strings to the single-letter codes used
// throughout the app ("f", "m", "c", "t").
func mapGender(g string) string {
	switch g {
	case "female":
		return "f"
	case "male":
		return "m"
	case "couple":
		return "c"
	case "trans":
		return "t"
	default:
		return g
	}
}

// FetchStream implements site.Site. Returns StreamInfo when online, nil when offline.
func (s *Stripchat) FetchStream(ctx context.Context, req *internal.Req, username string) (*site.StreamInfo, error) {
	apiURL := fmt.Sprintf("https://stripchat.com/api/front/v2/models/username/%s/cam", username)

	// Build request with required Stripchat headers.
	httpReq, cancel, err := req.CreateRequest(ctx, apiURL)
	if err != nil {
		return nil, fmt.Errorf("stripchat: create request: %w", err)
	}
	defer cancel()

	httpReq.Header.Set("Referer", fmt.Sprintf("https://stripchat.com/%s", username))
	httpReq.Header.Set("X-Requested-With", "XMLHttpRequest")

	body, err := req.DoRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("stripchat: fetch cam: %w", err)
	}

	var resp camResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("stripchat: parse cam response: %w", err)
	}

	u := resp.User.User
	info := &site.StreamInfo{
		RoomTitle:        resp.Cam.Topic,
		Gender:           mapGender(u.BroadcastGender),
		NumViewers:       0,
		SummaryCardImage: u.PreviewUrlThumbBig,
		CDNReferer:       "https://stripchat.com/",
		MouflonPDKey:     "auto",
	}

	if u.SnapshotTimestamp > 0 && resp.Cam.StreamName != "" {
		info.LiveThumbURL = fmt.Sprintf(
			"https://img.doppiocdn.net/thumbs/%d/%s",
			u.SnapshotTimestamp, resp.Cam.StreamName,
		)
	}

	// Offline if user is not online or not in public status.
	if !u.IsOnline || u.Status != "public" {
		return info, internal.ErrChannelOffline
	}
	if !resp.Cam.IsCamActive {
		return info, internal.ErrChannelOffline
	}

	streamName := resp.Cam.StreamName

	// Prefer the flashphoner-hls CDN server URL — this is a standard HLS master
	// playlist that does not use MOUFLON encryption. Fall back to the LL-HLS
	// edge-hls URL (which requires MOUFLON decryption) if viewServers is absent.
	var hlsURL string
	if server, ok := resp.Cam.ViewServers["flashphoner-hls"]; ok && server != "" {
		hlsURL = fmt.Sprintf(
			"https://b-%s.doppiocdn.com/hls/%s/master_%s.m3u8",
			server, streamName, streamName,
		)
	} else {
		hlsURL = fmt.Sprintf(
			"https://edge-hls.doppiocdn.net/hls/%s/master/%s_auto.m3u8?playlistType=lowLatency",
			streamName, streamName,
		)
	}

	// Build a live-updating thumbnail URL from snapshotTimestamp + streamName.
	// Pattern: https://img.doppiocdn.net/thumbs/{snapshotTimestamp}/{streamName}
	// MouflonPDKey is resolved later in FetchPlaylist once we have the pkey
	// from the master playlist's #EXT-X-MOUFLON:PSCH:v2:{pkey} line.
	// Pass "auto" to signal that FetchPlaylist should resolve it.
	info.HLSURL = hlsURL
	return info, nil
}

// FetchLastBroadcast implements site.Site. Stripchat has no equivalent endpoint.
func (s *Stripchat) FetchLastBroadcast(_ context.Context, _ *internal.Req, _ string) (int64, error) {
	return 0, nil
}

// ensure Stripchat implements site.Site at compile time.
var _ site.Site = (*Stripchat)(nil)
