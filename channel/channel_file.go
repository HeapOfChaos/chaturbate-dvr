package channel

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HeapOfChaos/goondvr/chaturbate"
)

// Pattern holds the date/time and sequence information for the filename pattern
type Pattern struct {
	Username string
	Year     string
	Month    string
	Day      string
	Hour     string
	Minute   string
	Second   string
	Sequence int
}

// NextFile prepares the next file to be created, by cleaning up the last file and generating a new one.
// ext is the file extension to use (e.g. ".ts" or ".mp4").
func (ch *Channel) NextFile(ext string) error {
	if err := ch.Cleanup(); err != nil {
		return err
	}
	filename, err := ch.GenerateFilename()
	if err != nil {
		return err
	}
	if err := ch.CreateNewFile(filename, ext); err != nil {
		return err
	}

	// Increment the sequence number for the next file
	ch.Sequence++
	return nil
}

// Cleanup cleans the file and resets it, called when the stream errors out or before next file was created.
func (ch *Channel) Cleanup() error {
	if ch.File == nil {
		return nil
	}
	filename := ch.File.Name()

	defer func() {
		ch.Filesize = 0
		ch.Duration = 0
	}()

	// Sync the file to ensure data is written to disk
	if err := ch.File.Sync(); err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("sync file: %w", err)
	}
	if err := ch.File.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("close file: %w", err)
	}
	ch.File = nil

	// Delete the empty file
	fileInfo, err := os.Stat(filename)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat file delete zero file: %w", err)
	}
	if fileInfo != nil && fileInfo.Size() == 0 {
		if err := os.Remove(filename); err != nil {
			return fmt.Errorf("remove zero file: %w", err)
		}
	} else if fileInfo != nil && strings.HasSuffix(filename, ".mp4") {
		// Append mfra seek index so players like VLC can seek instantly.
		// Run in background so it does not block the recording of the next file.
		go func() {
			if err := chaturbate.BuildSeekIndex(filename); err != nil {
				log.Printf("WARN  seek index %s: %v", filename, err)
			}
		}()
	}

	// Rescan total disk usage after a file is closed so the UI stays current.
	go ch.ScanTotalDiskUsage()
	return nil
}

// GenerateFilename creates a filename based on the configured pattern and the current timestamp
func (ch *Channel) GenerateFilename() (string, error) {
	var buf bytes.Buffer

	// Parse the filename pattern defined in the channel's config
	tpl, err := template.New("filename").Parse(ch.Config.Pattern)
	if err != nil {
		return "", fmt.Errorf("filename pattern error: %w", err)
	}

	// Get the current time based on the Unix timestamp when the stream was started
	t := time.Unix(ch.StreamedAt, 0)
	pattern := &Pattern{
		Username: ch.Config.Username,
		Sequence: ch.Sequence,
		Year:     t.Format("2006"),
		Month:    t.Format("01"),
		Day:      t.Format("02"),
		Hour:     t.Format("15"),
		Minute:   t.Format("04"),
		Second:   t.Format("05"),
	}

	if err := tpl.Execute(&buf, pattern); err != nil {
		return "", fmt.Errorf("template execution error: %w", err)
	}
	return buf.String(), nil
}

// CreateNewFile creates a new file for the channel using the given filename and extension.
func (ch *Channel) CreateNewFile(filename, ext string) error {

	// Ensure the directory exists before creating the file
	if err := os.MkdirAll(filepath.Dir(filename), 0777); err != nil {
		return fmt.Errorf("mkdir all: %w", err)
	}

	// Open the file in append mode, create it if it doesn't exist
	file, err := os.OpenFile(filename+ext, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0777)
	if err != nil {
		return fmt.Errorf("cannot open file: %s: %w", filename, err)
	}

	ch.File = file
	return nil
}

// recordingDirFromPattern extracts the base directory from a filename pattern
// like "videos/{{.Username}}_..." → "videos".
func recordingDirFromPattern(pattern string) string {
	idx := strings.Index(pattern, "{{")
	if idx == -1 {
		return "."
	}
	dir := filepath.Dir(pattern[:idx])
	if dir == "" || dir == "." {
		return "."
	}
	return dir
}

// ScanTotalDiskUsage calculates the total bytes of all recordings for this channel
// by walking the recording directory for files whose name starts with the username.
// The result is stored in TotalDiskUsageBytes.
func (ch *Channel) ScanTotalDiskUsage() {
	dir := recordingDirFromPattern(ch.Config.Pattern)
	prefix := ch.Config.Username
	var total int64
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(filepath.Base(path), prefix) {
			if info, err2 := d.Info(); err2 == nil {
				total += info.Size()
			}
		}
		return nil
	})
	ch.TotalDiskUsageBytes = total
}

// ShouldSwitchFile determines whether a new file should be created.
func (ch *Channel) ShouldSwitchFile() bool {
	maxFilesizeBytes := ch.Config.MaxFilesize * 1024 * 1024
	maxDurationSeconds := ch.Config.MaxDuration * 60

	return (ch.Duration >= float64(maxDurationSeconds) && ch.Config.MaxDuration > 0) ||
		(ch.Filesize >= maxFilesizeBytes && ch.Config.MaxFilesize > 0)
}
