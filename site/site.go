package site

import (
	"context"

	"github.com/HeapOfChaos/goondvr/internal"
)

// StreamInfo holds live stream data returned by a site when a channel is online.
type StreamInfo struct {
	HLSURL           string
	RoomTitle        string
	Gender           string
	NumViewers       int
	SummaryCardImage string
	LiveThumbURL     string // live-updating thumbnail URL; empty = use platform default (e.g. mmcdn)
	CDNReferer       string // Referer/Origin to use for CDN media requests; empty defaults to Chaturbate
	MouflonPDKey     string // Stripchat MOUFLON v2 decryption key; empty if not applicable
}

// Site defines the interface a streaming platform must implement.
type Site interface {
	// FetchStream returns StreamInfo if the channel is live, nil if offline.
	// Any non-nil error is a transient failure (retry). Offline is not an error — return nil, nil.
	FetchStream(ctx context.Context, req *internal.Req, username string) (*StreamInfo, error)
	// FetchLastBroadcast returns Unix timestamp of last broadcast, or 0 if unknown.
	FetchLastBroadcast(ctx context.Context, req *internal.Req, username string) (int64, error)
}
