package chaturbate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/grafov/m3u8"
	"github.com/samber/lo"
	"github.com/teacat/chaturbate-dvr/internal"
	"github.com/teacat/chaturbate-dvr/server"
)

type Client struct {
	Req *internal.Req
}

func NewClient() *Client {
	return &Client{Req: internal.NewReq()}
}

func (c *Client) GetStream(ctx context.Context, username string) (*Stream, error) {
	return FetchStream(ctx, c.Req, username)
}

type apiResponse struct {
	RoomStatus string `json:"room_status"`
	HLSSource  string `json:"hls_source"`
}

func FetchStream(ctx context.Context, client *internal.Req, username string) (*Stream, error) {
	apiURL := fmt.Sprintf("%sapi/chatvideocontext/%s/", server.Config.Domain, username)
	body, err := client.Get(ctx, apiURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get stream info: %w", err)
	}

	var resp apiResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse stream info: %w", err)
	}

	if server.Config.Debug {
		fmt.Printf("[DEBUG] API response for %s: room_status=%s hls_source=%v\n", username, resp.RoomStatus, resp.HLSSource != "")
	}

	if resp.HLSSource != "" {
		return &Stream{HLSSource: resp.HLSSource}, nil
	}

	if resp.RoomStatus == "private" {
		return nil, internal.ErrPrivateStream
	}

	return nil, internal.ErrChannelOffline
}

type Stream struct {
	HLSSource string
}

func (s *Stream) GetPlaylist(ctx context.Context, resolution, framerate int) (*Playlist, error) {
	return FetchPlaylist(ctx, s.HLSSource, resolution, framerate)
}

func FetchPlaylist(ctx context.Context, hlsSource string, resolution, framerate int) (*Playlist, error) {
	if hlsSource == "" {
		// The page loaded but the stream is not active — treat as offline.
		return nil, internal.ErrChannelOffline
	}

	client := internal.NewMediaReq()
	resp, err := client.Get(ctx, hlsSource)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch HLS source: %w", err)
	}

	playlist, err := ParsePlaylist(resp, hlsSource, resolution, framerate)
	if err != nil {
		return nil, err
	}
	playlist.Client = client
	return playlist, nil
}

func ParsePlaylist(resp, hlsSource string, resolution, framerate int) (*Playlist, error) {
	p, _, err := m3u8.DecodeFrom(strings.NewReader(resp), true)
	if err != nil {
		if server.Config.Debug {
			fmt.Printf("[DEBUG] master playlist parse failed: %v\n", err)
			fmt.Printf("[DEBUG]   HLS source URL: %s\n", hlsSource)
			end := len(resp)
			if end > 2000 {
				end = 2000
			}
			fmt.Printf("[DEBUG]   Response (first 2000 chars):\n%s\n", resp[:end])
		}
		return nil, fmt.Errorf("failed to decode m3u8 playlist: %w", err)
	}

	masterPlaylist, ok := p.(*m3u8.MasterPlaylist)
	if !ok {
		return nil, errors.New("invalid master playlist format")
	}

	return PickPlaylist(masterPlaylist, hlsSource, resolution, framerate)
}

// Playlist represents an HLS playlist containing variant streams.
type Playlist struct {
	PlaylistURL      string
	AudioPlaylistURL string        // LL-HLS audio rendition URL; empty for legacy streams
	RootURL          string        // base for resolving video segment URIs
	Resolution       int
	Framerate        int
	FileExt          string        // ".ts" for legacy HLS, ".mp4" for LL-HLS fMP4
	Client           *internal.Req // reuse the same client that fetched the master playlist
}

// VideoResolution represents a video resolution and its corresponding framerate URLs.
type VideoResolution struct {
	Framerate map[int]string // [framerate]url
	Width     int
}

// Resolution is a type alias kept for compatibility.
type Resolution = VideoResolution

func resolveHLSURL(base, ref string) string {
	baseClean := strings.SplitN(base, "?", 2)[0]
	baseURL, err := url.Parse(baseClean)
	if err != nil {
		return base + ref
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return base + ref
	}
	return baseURL.ResolveReference(refURL).String()
}

func PickPlaylist(masterPlaylist *m3u8.MasterPlaylist, baseURL string, resolution, framerate int) (*Playlist, error) {
	resolutions := map[int]*VideoResolution{}

	for _, v := range masterPlaylist.Variants {
		parts := strings.Split(v.Resolution, "x")
		if len(parts) != 2 {
			continue
		}
		width, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("parse resolution: %w", err)
		}
		framerateVal := 30
		if strings.Contains(v.Name, "FPS:60.0") {
			framerateVal = 60
		}
		if _, exists := resolutions[width]; !exists {
			resolutions[width] = &VideoResolution{Framerate: map[int]string{}, Width: width}
		}
		resolutions[width].Framerate[framerateVal] = v.URI
	}

	variant, exists := resolutions[resolution]
	if !exists {
		candidates := lo.Filter(lo.Values(resolutions), func(r *VideoResolution, _ int) bool {
			return r.Width < resolution
		})
		variant = lo.MaxBy(candidates, func(a, b *VideoResolution) bool {
			return a.Width > b.Width
		})
	}
	if variant == nil {
		return nil, fmt.Errorf("resolution not found")
	}

	var (
		finalResolution = variant.Width
		finalFramerate  = framerate
	)
	playlistURL, exists := variant.Framerate[framerate]
	if !exists {
		for fr, u := range variant.Framerate {
			playlistURL = u
			finalFramerate = fr
			break
		}
	}

	fileExt := ".ts"
	if strings.Contains(playlistURL, "llhls") || strings.HasSuffix(strings.SplitN(playlistURL, "?", 2)[0], ".m4s") {
		fileExt = ".mp4"
	}

	// For LL-HLS streams, find the audio rendition from the selected variant's EXT-X-MEDIA alternatives.
	var audioPlaylistURL string
	if fileExt == ".mp4" {
		for _, v := range masterPlaylist.Variants {
			if v.URI == playlistURL {
				for _, alt := range v.Alternatives {
					if alt != nil && alt.Type == "AUDIO" && alt.URI != "" {
						audioPlaylistURL = resolveHLSURL(baseURL, alt.URI)
						break
					}
				}
				break
			}
		}
		if server.Config.Debug {
			if audioPlaylistURL != "" {
				fmt.Printf("[DEBUG] LL-HLS audio rendition: %s\n", audioPlaylistURL)
			} else {
				fmt.Printf("[DEBUG] LL-HLS stream has no separate audio rendition\n")
			}
		}
	}

	return &Playlist{
		PlaylistURL:      resolveHLSURL(baseURL, playlistURL),
		AudioPlaylistURL: audioPlaylistURL,
		RootURL:          strings.SplitN(baseURL, "?", 2)[0],
		Resolution:       finalResolution,
		Framerate:        finalFramerate,
		FileExt:          fileExt,
	}, nil
}

// WatchHandler is a function type that processes video segments.
type WatchHandler func(b []byte, duration float64) error

// WatchSegments continuously fetches and processes video segments.
// For LL-HLS streams with a separate audio rendition it automatically muxes
// audio and video into a single fragmented MP4 output stream.
func (p *Playlist) WatchSegments(ctx context.Context, handler WatchHandler) error {
	if p.AudioPlaylistURL != "" {
		return p.watchMuxedSegments(ctx, handler)
	}
	return p.watchVideoOnlySegments(ctx, handler)
}

// watchVideoOnlySegments is the original single-track segment loop.
func (p *Playlist) watchVideoOnlySegments(ctx context.Context, handler WatchHandler) error {
	client := p.Client
	if client == nil {
		client = internal.NewMediaReq()
	}
	lastSeq := -1
	lastMapURI := ""
	consecutiveErrors := 0

	for {
		resp, err := client.Get(ctx, p.PlaylistURL)
		if err != nil {
			if consecutiveErrors++; consecutiveErrors >= 5 {
				return fmt.Errorf("get playlist: %w", err)
			}
			<-time.After(2 * time.Second)
			continue
		}
		pl, _, err := m3u8.DecodeFrom(strings.NewReader(resp), true)
		if err != nil {
			if server.Config.Debug {
				fmt.Printf("[DEBUG] variant playlist parse failed: %v\n", err)
				fmt.Printf("[DEBUG]   Playlist URL: %s\n", p.PlaylistURL)
				end := len(resp)
				if end > 2000 {
					end = 2000
				}
				fmt.Printf("[DEBUG]   Response (first 2000 chars):\n%s\n", resp[:end])
			}
			if consecutiveErrors++; consecutiveErrors >= 5 {
				return fmt.Errorf("decode from: %w", err)
			}
			<-time.After(2 * time.Second)
			continue
		}
		playlist, ok := pl.(*m3u8.MediaPlaylist)
		if !ok {
			return fmt.Errorf("cast to media playlist")
		}
		consecutiveErrors = 0

		for _, v := range playlist.Segments {
			if v == nil {
				continue
			}
			seq := internal.SegmentSeq(v.URI)
			if server.Config.Debug && lastSeq == -1 {
				fmt.Printf("[DEBUG] first segment URI: %s (seq=%d)\n", v.URI, seq)
			}
			if seq == -1 || seq <= lastSeq {
				continue
			}
			if v.Map != nil && v.Map.URI != lastMapURI {
				mapURL := resolveHLSURL(p.RootURL, v.Map.URI)
				initBytes, err := client.GetBytes(ctx, mapURL)
				if err != nil {
					return fmt.Errorf("get init segment: %w", err)
				}
				if err := handler(initBytes, 0); err != nil {
					return fmt.Errorf("handler init segment: %w", err)
				}
				lastMapURI = v.Map.URI
			}

			lastSeq = seq

			pipeline := func() ([]byte, error) {
				return client.GetBytes(ctx, resolveHLSURL(p.RootURL, v.URI))
			}
			resp, err := retry.DoWithData(
				pipeline,
				retry.Context(ctx),
				retry.Attempts(3),
				retry.Delay(600*time.Millisecond),
				retry.DelayType(retry.FixedDelay),
			)
			if err != nil {
				break
			}
			if err := handler(resp, v.Duration); err != nil {
				return fmt.Errorf("handler: %w", err)
			}
		}

		<-time.After(1 * time.Second)
	}
}

// watchMuxedSegments polls both video and audio LL-HLS playlists, combines their
// init segments into a single fMP4 init, then writes interleaved video and
// audio moof+mdat fragments. Audio track_id is renumbered to 2.
// tfdt decode times are normalised to start from zero so players display the
// correct recording position rather than the CDN stream uptime offset.
func (p *Playlist) watchMuxedSegments(ctx context.Context, handler WatchHandler) error {
	client := p.Client
	if client == nil {
		client = internal.NewMediaReq()
	}

	lastVideoSeq := -1
	lastAudioSeq := -1
	lastVideoURI := ""
	lastAudioURI := ""
	lastVideoMapURI := ""
	lastAudioMapURI := ""
	var videoInitBytes []byte
	var audioInitBytes []byte
	initWritten := false
	consecutiveErrors := 0

	// Per-track tfdt base times captured from the first segment of each track.
	// Subtracting these normalises timestamps to start from zero.
	var videoTimeBase uint64
	var audioTimeBase uint64
	videoBaseSet := false
	audioBaseSet := false

	for {
		// Fetch video playlist
		videoResp, err := client.Get(ctx, p.PlaylistURL)
		if err != nil {
			if consecutiveErrors++; consecutiveErrors >= 5 {
				return fmt.Errorf("get video playlist: %w", err)
			}
			<-time.After(2 * time.Second)
			continue
		}
		vpl, _, err := m3u8.DecodeFrom(strings.NewReader(videoResp), true)
		if err != nil {
			if server.Config.Debug {
				fmt.Printf("[DEBUG] muxed: video playlist parse failed: %v\n", err)
			}
			if consecutiveErrors++; consecutiveErrors >= 5 {
				return fmt.Errorf("decode video playlist: %w", err)
			}
			<-time.After(2 * time.Second)
			continue
		}
		videoPlaylist, ok := vpl.(*m3u8.MediaPlaylist)
		if !ok {
			return fmt.Errorf("cast video playlist to media playlist")
		}

		// Fetch audio playlist
		audioResp, err := client.Get(ctx, p.AudioPlaylistURL)
		if err != nil {
			if consecutiveErrors++; consecutiveErrors >= 5 {
				return fmt.Errorf("get audio playlist: %w", err)
			}
			<-time.After(2 * time.Second)
			continue
		}
		apl, _, err := m3u8.DecodeFrom(strings.NewReader(audioResp), true)
		if err != nil {
			if server.Config.Debug {
				fmt.Printf("[DEBUG] muxed: audio playlist parse failed: %v\n", err)
			}
			if consecutiveErrors++; consecutiveErrors >= 5 {
				return fmt.Errorf("decode audio playlist: %w", err)
			}
			<-time.After(2 * time.Second)
			continue
		}
		audioPlaylist, ok := apl.(*m3u8.MediaPlaylist)
		if !ok {
			return fmt.Errorf("cast audio playlist to media playlist")
		}
		consecutiveErrors = 0

		// Collect video init segment (EXT-X-MAP)
		for _, v := range videoPlaylist.Segments {
			if v == nil {
				continue
			}
			if v.Map != nil && v.Map.URI != lastVideoMapURI {
				mapURL := resolveHLSURL(p.RootURL, v.Map.URI)
				b, err := client.GetBytes(ctx, mapURL)
				if err != nil {
					return fmt.Errorf("get video init segment: %w", err)
				}
				videoInitBytes = b
				lastVideoMapURI = v.Map.URI
				initWritten = false
			}
			break
		}

		// Collect audio init segment (EXT-X-MAP)
		for _, v := range audioPlaylist.Segments {
			if v == nil {
				continue
			}
			if v.Map != nil && v.Map.URI != lastAudioMapURI {
				mapURL := resolveHLSURL(p.AudioPlaylistURL, v.Map.URI)
				b, err := client.GetBytes(ctx, mapURL)
				if err != nil {
					return fmt.Errorf("get audio init segment: %w", err)
				}
				audioInitBytes = b
				lastAudioMapURI = v.Map.URI
				initWritten = false
			}
			break
		}

		// Write combined init once we have both init segments
		if !initWritten && videoInitBytes != nil && audioInitBytes != nil {
			combined, err := buildCombinedInit(videoInitBytes, audioInitBytes)
			if err != nil {
				return fmt.Errorf("build combined init: %w", err)
			}
			if err := handler(combined, 0); err != nil {
				return fmt.Errorf("handler combined init: %w", err)
			}
			initWritten = true
		}
		if !initWritten {
			<-time.After(1 * time.Second)
			continue
		}

		// Collect new segment URLs. Pre-resolve URLs to avoid closure capture
		// issues, and fall back to URI-string dedup when seq is unavailable.
		type segInfo struct {
			url      string
			duration float64
		}
		var newVideoSegs []segInfo
		for _, v := range videoPlaylist.Segments {
			if v == nil {
				continue
			}
			seq := internal.SegmentSeq(v.URI)
			if server.Config.Debug && lastVideoSeq == -1 && lastVideoURI == "" {
				fmt.Printf("[DEBUG] muxed: first video segment URI: %s (seq=%d)\n", v.URI, seq)
			}
			if seq != -1 {
				if seq <= lastVideoSeq {
					continue
				}
				lastVideoSeq = seq
			} else {
				if v.URI == lastVideoURI {
					continue
				}
			}
			lastVideoURI = v.URI
			newVideoSegs = append(newVideoSegs, segInfo{
				url:      resolveHLSURL(p.RootURL, v.URI),
				duration: v.Duration,
			})
		}
		var newAudioSegs []segInfo
		for _, v := range audioPlaylist.Segments {
			if v == nil {
				continue
			}
			seq := internal.SegmentSeq(v.URI)
			if server.Config.Debug && lastAudioSeq == -1 && lastAudioURI == "" {
				fmt.Printf("[DEBUG] muxed: first audio segment URI: %s (seq=%d)\n", v.URI, seq)
			}
			if seq != -1 {
				if seq <= lastAudioSeq {
					continue
				}
				lastAudioSeq = seq
			} else {
				if v.URI == lastAudioURI {
					continue
				}
			}
			lastAudioURI = v.URI
			newAudioSegs = append(newAudioSegs, segInfo{
				url:      resolveHLSURL(p.AudioPlaylistURL, v.URI),
				duration: v.Duration,
			})
		}

		if server.Config.Debug {
				fmt.Printf("[DEBUG] muxed: cycle video=%d audio=%d\n", len(newVideoSegs), len(newAudioSegs))
		}

		// Interleave video and audio: write V1, A1, V2, A2, ...
		// This gives players a balanced buffer of both tracks and avoids
		// choppy audio caused by large runs of video-only data.
		maxLen := len(newVideoSegs)
		if len(newAudioSegs) > maxLen {
			maxLen = len(newAudioSegs)
		}
		for i := 0; i < maxLen; i++ {
			if i < len(newVideoSegs) {
				vseg := newVideoSegs[i]
				vsegURL := vseg.url
				segBytes, err := retry.DoWithData(
					func() ([]byte, error) { return client.GetBytes(ctx, vsegURL) },
					retry.Context(ctx),
					retry.Attempts(3),
					retry.Delay(600*time.Millisecond),
					retry.DelayType(retry.FixedDelay),
				)
				if err == nil {
					if !videoBaseSet {
						if t, ok := extractMoofFirstTfdt(segBytes); ok {
							videoTimeBase = t
							videoBaseSet = true
						}
					}
					segBytes = shiftSegmentTfdt(segBytes, 1, videoTimeBase)
					if err := handler(segBytes, vseg.duration); err != nil {
						return fmt.Errorf("handler video segment: %w", err)
					}
				}
			}
			if i < len(newAudioSegs) {
				aseg := newAudioSegs[i]
				asegURL := aseg.url
				segBytes, err := retry.DoWithData(
					func() ([]byte, error) { return client.GetBytes(ctx, asegURL) },
					retry.Context(ctx),
					retry.Attempts(3),
					retry.Delay(600*time.Millisecond),
					retry.DelayType(retry.FixedDelay),
				)
				if err != nil {
					fmt.Printf("[WARN] audio seg download failed: %v\n", err)
				} else {
					if !audioBaseSet {
						if t, ok := extractMoofFirstTfdt(segBytes); ok {
							audioTimeBase = t
							audioBaseSet = true
							if server.Config.Debug {
								fmt.Printf("[DEBUG] muxed: audio base=%d\n", audioTimeBase)
							}
						}
					}
					if server.Config.Debug {
						if rawTfdt, ok := extractMoofFirstTfdt(segBytes); ok {
							var normalised uint64
							if audioTimeBase > 0 && rawTfdt >= audioTimeBase {
								normalised = rawTfdt - audioTimeBase
							}
							fmt.Printf("[DEBUG] muxed: audio seg dur=%.3f raw_tfdt=%d norm=%d\n", aseg.duration, rawTfdt, normalised)
						}
					}
					segBytes = rewriteAudioMoofTrackID(segBytes)
					segBytes = shiftSegmentTfdt(segBytes, 2, audioTimeBase)
					if err := handler(segBytes, 0); err != nil {
						return fmt.Errorf("handler audio segment: %w", err)
					}
				}
			}
		}

		<-time.After(1 * time.Second)
	}
}
