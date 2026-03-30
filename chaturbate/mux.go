package chaturbate

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
)

// mp4RawBox is a parsed MP4 box (header + payload as a single byte slice).
type mp4RawBox struct {
	typ  string
	data []byte // full box bytes, including the 8-byte size+type header
}

// parseMP4Boxes parses a flat sequence of MP4 boxes from data.
// data must NOT include a parent box header - it's the raw child content.
func parseMP4Boxes(data []byte) ([]mp4RawBox, error) {
	var boxes []mp4RawBox
	pos := 0
	for pos+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[pos:]))
		if size < 8 {
			return nil, fmt.Errorf("invalid box size %d at offset %d", size, pos)
		}
		if pos+size > len(data) {
			return nil, fmt.Errorf("box extends past data: size=%d pos=%d len=%d", size, pos, len(data))
		}
		boxes = append(boxes, mp4RawBox{
			typ:  string(data[pos+4 : pos+8]),
			data: data[pos : pos+size],
		})
		pos += size
	}
	return boxes, nil
}

// findMP4Box returns the first box of typ in data (raw child content, no parent header).
func findMP4Box(data []byte, typ string) ([]byte, bool) {
	boxes, err := parseMP4Boxes(data)
	if err != nil {
		return nil, false
	}
	for _, b := range boxes {
		if b.typ == typ {
			return b.data, true
		}
	}
	return nil, false
}

// makeMP4Box wraps content in a new box with the given four-character type.
func makeMP4Box(typ string, content []byte) []byte {
	size := uint32(8 + len(content))
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b[0:], size)
	copy(b[4:], typ)
	copy(b[8:], content)
	return b
}

// buildCombinedInit merges a video-only and audio-only CMAF init segment into a
// single fragmented MP4 init segment. Video keeps track_id=1; audio gets track_id=2.
// The resulting file is playable by VLC and ffmpeg.
func buildCombinedInit(videoInit, audioInit []byte) ([]byte, error) {
	// Locate top-level boxes in each init segment
	ftypBox, hasFtyp := findMP4Box(videoInit, "ftyp")
	videoMoovBox, ok := findMP4Box(videoInit, "moov")
	if !ok {
		return nil, fmt.Errorf("video init missing moov box")
	}
	audioMoovBox, ok := findMP4Box(audioInit, "moov")
	if !ok {
		return nil, fmt.Errorf("audio init missing moov box")
	}

	// Parse children of each moov (skip the 8-byte "moov" box header)
	videoMoovChildren := videoMoovBox[8:]
	audioMoovChildren := audioMoovBox[8:]

	videoBoxes, err := parseMP4Boxes(videoMoovChildren)
	if err != nil {
		return nil, fmt.Errorf("parse video moov children: %w", err)
	}
	audioBoxes, err := parseMP4Boxes(audioMoovChildren)
	if err != nil {
		return nil, fmt.Errorf("parse audio moov children: %w", err)
	}

	// Extract named children from video moov
	var mvhdBox, videoTrakBox, videoMvexBox []byte
	for _, b := range videoBoxes {
		switch b.typ {
		case "mvhd":
			mvhdBox = b.data
		case "trak":
			videoTrakBox = b.data
		case "mvex":
			videoMvexBox = b.data
		}
	}
	// Extract named children from audio moov
	var audioTrakBox, audioMvexBox []byte
	for _, b := range audioBoxes {
		switch b.typ {
		case "trak":
			audioTrakBox = b.data
		case "mvex":
			audioMvexBox = b.data
		}
	}

	if mvhdBox == nil || videoTrakBox == nil {
		return nil, fmt.Errorf("video moov missing mvhd or trak")
	}
	if audioTrakBox == nil {
		return nil, fmt.Errorf("audio moov missing trak")
	}

	// Patch mvhd: zero duration (computed from fragments) and set next_track_id=3.
	mvhdPatched := make([]byte, len(mvhdBox))
	copy(mvhdPatched, mvhdBox)
	if len(mvhdPatched) > 8 {
		mvhdVersion := mvhdPatched[8]
		if mvhdVersion == 0 && len(mvhdPatched) >= 28 {
			binary.BigEndian.PutUint32(mvhdPatched[24:], 0) // duration
		} else if mvhdVersion == 1 && len(mvhdPatched) >= 40 {
			binary.BigEndian.PutUint64(mvhdPatched[32:], 0) // duration
		}
	}
	if len(mvhdPatched) >= 4 {
		binary.BigEndian.PutUint32(mvhdPatched[len(mvhdPatched)-4:], 3) // next_track_id
	}

	// Patch video trak: zero tkhd.duration (CDN may have a stale value).
	videoTrakBox = zeroDurationsInTrak(videoTrakBox)

	// Patch audio trak: change tkhd.track_id to 2 and zero duration.
	audioTrakPatched := zeroDurationsInTrak(patchTrakTrackID(audioTrakBox, 2))

	// Build combined mvex: video trex (track_id=1) + audio trex (track_id=2)
	var combinedMvex []byte
	if videoMvexBox != nil || audioMvexBox != nil {
		var mvexContent []byte
		if videoMvexBox != nil {
			mvexContent = append(mvexContent, videoMvexBox[8:]...)
		}
		if audioMvexBox != nil {
			audioTrexContent := patchTrexTrackIDs(audioMvexBox[8:], 2)
			mvexContent = append(mvexContent, audioTrexContent...)
		}
		combinedMvex = makeMP4Box("mvex", mvexContent)
	}

	// Assemble new moov content: mvhd + video trak + audio trak + mvex
	var moovContent []byte
	moovContent = append(moovContent, mvhdPatched...)
	moovContent = append(moovContent, videoTrakBox...)
	moovContent = append(moovContent, audioTrakPatched...)
	if combinedMvex != nil {
		moovContent = append(moovContent, combinedMvex...)
	}

	// Write result: optional ftyp then new moov
	var out []byte
	if hasFtyp {
		out = append(out, ftypBox...)
	}
	out = append(out, makeMP4Box("moov", moovContent)...)
	return out, nil
}

// patchTrakTrackID returns a copy of a trak box with tkhd.track_id replaced by newID.
func patchTrakTrackID(trakBox []byte, newID uint32) []byte {
	result := make([]byte, len(trakBox))
	copy(result, trakBox)
	// trak children start at offset 8 (after size+type header)
	trakContent := result[8:]
	boxes, _ := parseMP4Boxes(trakContent)
	offset := 0
	for _, b := range boxes {
		if b.typ == "tkhd" {
			// tkhd layout:
			//   [4] size  [4] "tkhd"  [1] version  [3] flags
			//   version 0: [4] ctime [4] mtime -> track_id at byte 20 from box start
			//   version 1: [8] ctime [8] mtime -> track_id at byte 28 from box start
			if offset+9 > len(trakContent) {
				break
			}
			version := trakContent[offset+8]
			var trackIDOff int
			if version == 0 {
				trackIDOff = offset + 20 // 8+1+3+4+4
			} else {
				trackIDOff = offset + 28 // 8+1+3+8+8
			}
			if trackIDOff+4 <= len(trakContent) {
				binary.BigEndian.PutUint32(trakContent[trackIDOff:], newID)
			}
			break
		}
		offset += len(b.data)
	}
	return result
}

// patchTrexTrackIDs returns a copy of mvex child content with all trex.track_id
// fields replaced by newID.
func patchTrexTrackIDs(mvexContent []byte, newID uint32) []byte {
	result := make([]byte, len(mvexContent))
	copy(result, mvexContent)
	boxes, _ := parseMP4Boxes(result)
	offset := 0
	for _, b := range boxes {
		if b.typ == "trex" {
			// trex: [4 size][4 "trex"][1 version][3 flags][4 track_id]
			if offset+16 <= len(result) {
				binary.BigEndian.PutUint32(result[offset+12:], newID)
			}
		}
		offset += len(b.data)
	}
	return result
}


// zeroDurationsInTrak returns a copy of trakBox with tkhd.duration and mdhd.duration
// both set to 0. For fragmented MP4 recordings the actual duration comes from
// the fragment timestamps, so keeping CDN-supplied stale durations in the init
// segment causes players to show incorrect total-duration / seek-bar length.
func zeroDurationsInTrak(trakBox []byte) []byte {
	result := make([]byte, len(trakBox))
	copy(result, trakBox)
	trakContent := result[8:]
	boxes, _ := parseMP4Boxes(trakContent)
	offset := 0
	for _, b := range boxes {
		switch b.typ {
		case "tkhd":
			if offset+9 <= len(trakContent) {
				version := trakContent[offset+8]
				// v0: [8 hdr+ver+flags][4 ctime][4 mtime][4 track_id][4 reserved][4 duration]
				// v1: [8 hdr+ver+flags][8 ctime][8 mtime][4 track_id][4 reserved][8 duration]
				var durOff int
				if version == 0 {
					durOff = offset + 28
				} else {
					durOff = offset + 36
				}
				if version == 0 && durOff+4 <= len(trakContent) {
					binary.BigEndian.PutUint32(trakContent[durOff:], 0)
				} else if version == 1 && durOff+8 <= len(trakContent) {
					binary.BigEndian.PutUint64(trakContent[durOff:], 0)
				}
			}
		case "mdia":
			// Parse mdia content (slice of result, so edits propagate).
			mdiaContent := trakContent[offset+8 : offset+len(b.data)]
			mdiaBoxes, _ := parseMP4Boxes(mdiaContent)
			mdiaOff := 0
			for _, mb := range mdiaBoxes {
				if mb.typ == "mdhd" && mdiaOff+9 <= len(mdiaContent) {
					version := mdiaContent[mdiaOff+8]
					// mdhd layout identical to mvhd: duration at offset 24 (v0) or 32 (v1)
					var durOff int
					if version == 0 {
						durOff = mdiaOff + 24
					} else {
						durOff = mdiaOff + 32
					}
					if version == 0 && durOff+4 <= len(mdiaContent) {
						binary.BigEndian.PutUint32(mdiaContent[durOff:], 0)
					} else if version == 1 && durOff+8 <= len(mdiaContent) {
						binary.BigEndian.PutUint64(mdiaContent[durOff:], 0)
					}
					break
				}
				mdiaOff += len(mb.data)
			}
		}
		offset += len(b.data)
	}
	return result
}

// rewriteAudioMoofTrackID returns a copy of segment bytes with all track ID
// references changed to 2.  This covers tfhd inside moof/traf and also any
// top-level sidx.reference_ID box (CMAF segments often start with sidx).
func rewriteAudioMoofTrackID(data []byte) []byte {
	result := make([]byte, len(data))
	copy(result, data)
	pos := 0
	for pos+8 <= len(result) {
		size := int(binary.BigEndian.Uint32(result[pos:]))
		if size < 8 || pos+size > len(result) {
			break
		}
		switch string(result[pos+4 : pos+8]) {
		case "sidx":
			// sidx: [4 size][4 "sidx"][1 version][3 flags][4 reference_ID]
			if pos+16 <= len(result) {
				binary.BigEndian.PutUint32(result[pos+12:], 2)
			}
		case "moof":
			scanAndPatchTfhd(result[pos+8:pos+size], 2)
		}
		pos += size
	}
	return result
}

// scanAndPatchTfhd recursively walks MP4 boxes in data and patches
// the first tfhd box it finds, setting track_id = newID.
// data is modified in-place.
func scanAndPatchTfhd(data []byte, newID uint32) bool {
	pos := 0
	for pos+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[pos:]))
		if size < 8 || pos+size > len(data) {
			break
		}
		boxType := string(data[pos+4 : pos+8])
		switch boxType {
		case "tfhd":
			// tfhd: [4 size][4 "tfhd"][1 version][3 flags][4 track_id]
			if pos+16 <= len(data) {
				binary.BigEndian.PutUint32(data[pos+12:], newID)
			}
			return true
		case "moof", "traf":
			// Recurse into container box children (skip the 8-byte header)
			if scanAndPatchTfhd(data[pos+8:pos+size], newID) {
				return true
			}
		}
		pos += size
	}
	return false
}

// tfraEntry records the moof byte-offset and first-sample decode time for one fragment.
type tfraEntry struct {
	time   uint64 // baseMediaDecodeTime from tfdt
	offset uint64 // byte offset of the moof box from start of file
}

// tfdtPatch records where a tfdt time value lives in the file so it can be
// rewritten in-place without loading the entire file into memory.
type tfdtPatch struct {
	fileOffset int64  // absolute file offset of the time field (4 or 8 bytes)
	version    byte   // 0 = 32-bit, 1 = 64-bit
	trackID    uint32 // which track this tfdt belongs to
}

// BuildSeekIndex scans a completed fragmented MP4 file, normalises tfdt
// decode times to start from zero (so VLC displays correct timestamps
// instead of the live-stream's wall-clock offset, e.g. "44:00"), builds an
// mfra (Movie Fragment Random Access) box containing tfra entries for every
// moof fragment, and appends it to the file.  It is a no-op if the file
// already has an mfra box or contains no moof boxes.
//
// The scan is streaming: only one moof box is held in memory at a time,
// so memory usage is O(number of fragments), not O(file size).
func BuildSeekIndex(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	fileSize, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek end: %w", err)
	}
	if fileSize == 0 {
		return nil
	}

	trackEntries := map[uint32][]tfraEntry{} // track_id -> entries
	var patches []tfdtPatch

	// Pass 1: walk top-level boxes by reading 8-byte headers and seeking.
	// Only moof boxes are read into memory (typically a few KB each).
	var pos int64
	hdr := make([]byte, 8)
	for pos+8 <= fileSize {
		if _, err := f.ReadAt(hdr, pos); err != nil {
			break
		}
		size := int64(binary.BigEndian.Uint32(hdr[0:4]))
		boxType := string(hdr[4:8])
		if size < 8 || pos+size > fileSize {
			break
		}

		if boxType == "mfra" {
			return nil // already indexed
		}

		if boxType == "moof" {
			moofData := make([]byte, size-8)
			if _, err := f.ReadAt(moofData, pos+8); err != nil {
				break
			}
			trackID, decodeTime, tfdtPatches := scanMoofForIndex(moofData, pos)
			if trackID != 0 {
				trackEntries[trackID] = append(trackEntries[trackID], tfraEntry{
					time:   decodeTime,
					offset: uint64(pos),
				})
			}
			patches = append(patches, tfdtPatches...)
		}
		pos += size
	}

	if len(trackEntries) == 0 {
		return nil
	}

	// Compute per-track minimum baseMediaDecodeTime.
	minTimes := map[uint32]uint64{}
	for id, entries := range trackEntries {
		min := entries[0].time
		for _, e := range entries[1:] {
			if e.time < min {
				min = e.time
			}
		}
		minTimes[id] = min
	}

	// Pass 2: patch tfdt values in-place using pwrite (no full-file rewrite).
	for _, p := range patches {
		minT := minTimes[p.trackID]
		if minT == 0 {
			continue
		}
		if p.version == 1 {
			var buf [8]byte
			if _, err := f.ReadAt(buf[:], p.fileOffset); err != nil {
				continue
			}
			cur := binary.BigEndian.Uint64(buf[:])
			if cur >= minT {
				binary.BigEndian.PutUint64(buf[:], cur-minT)
				f.WriteAt(buf[:], p.fileOffset)
			}
		} else {
			var buf [4]byte
			if _, err := f.ReadAt(buf[:], p.fileOffset); err != nil {
				continue
			}
			cur := uint64(binary.BigEndian.Uint32(buf[:]))
			if cur >= minT {
				binary.BigEndian.PutUint32(buf[:], uint32(cur-minT))
				f.WriteAt(buf[:], p.fileOffset)
			}
		}
	}

	// Adjust collected entry times to match rewritten values.
	for id := range trackEntries {
		minT := minTimes[id]
		for i := range trackEntries[id] {
			if trackEntries[id][i].time >= minT {
				trackEntries[id][i].time -= minT
			}
		}
	}

	// Append mfra box at end of file.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek to end: %w", err)
	}
	mfraBox := buildMFRABox(trackEntries)
	if _, err := f.Write(mfraBox); err != nil {
		return fmt.Errorf("write mfra: %w", err)
	}
	return nil
}

// scanMoofForIndex parses moof content (without the 8-byte box header) and
// returns the first track's ID and decode time, plus patch records for every
// tfdt found. moofFileOffset is the absolute file offset of the moof box.
func scanMoofForIndex(moofContent []byte, moofFileOffset int64) (uint32, uint64, []tfdtPatch) {
	var firstTrackID uint32
	var firstDecodeTime uint64
	var patches []tfdtPatch

	// moofContent starts at moofFileOffset+8 in the file.
	baseOffset := moofFileOffset + 8

	inner := 0
	for inner+8 <= len(moofContent) {
		innerSize := int(binary.BigEndian.Uint32(moofContent[inner:]))
		if innerSize < 8 || inner+innerSize > len(moofContent) {
			break
		}
		if string(moofContent[inner+4:inner+8]) == "traf" {
			trafContent := moofContent[inner+8 : inner+innerSize]
			trafFileOffset := baseOffset + int64(inner) + 8
			trackID, decodeTime, patch := scanTrafForIndex(trafContent, trafFileOffset)
			if trackID != 0 {
				if firstTrackID == 0 {
					firstTrackID = trackID
					firstDecodeTime = decodeTime
				}
				if patch != nil {
					patches = append(patches, *patch)
				}
			}
		}
		inner += innerSize
	}
	return firstTrackID, firstDecodeTime, patches
}

// scanTrafForIndex reads tfhd (track_id) and tfdt (decode time + file offset)
// from a single traf box's content. Returns a tfdtPatch if a tfdt was found.
func scanTrafForIndex(trafContent []byte, trafFileOffset int64) (uint32, uint64, *tfdtPatch) {
	var trackID uint32
	var decodeTime uint64
	var patch *tfdtPatch

	pos := 0
	for pos+8 <= len(trafContent) {
		size := int(binary.BigEndian.Uint32(trafContent[pos:]))
		if size < 8 || pos+size > len(trafContent) {
			break
		}
		boxType := string(trafContent[pos+4 : pos+8])
		switch boxType {
		case "tfhd":
			if pos+16 <= len(trafContent) {
				trackID = binary.BigEndian.Uint32(trafContent[pos+12:])
			}
		case "tfdt":
			if pos+9 <= len(trafContent) {
				version := trafContent[pos+8]
				if version == 1 && pos+20 <= len(trafContent) {
					decodeTime = binary.BigEndian.Uint64(trafContent[pos+12:])
					patch = &tfdtPatch{
						fileOffset: trafFileOffset + int64(pos) + 12,
						version:    1,
					}
				} else if version == 0 && pos+16 <= len(trafContent) {
					decodeTime = uint64(binary.BigEndian.Uint32(trafContent[pos+12:]))
					patch = &tfdtPatch{
						fileOffset: trafFileOffset + int64(pos) + 12,
						version:    0,
					}
				}
			}
		}
		pos += size
	}
	if patch != nil {
		patch.trackID = trackID
	}
	return trackID, decodeTime, patch
}

// normaliseTfdt subtracts the per-track minimum baseMediaDecodeTime from
// every tfdt box found inside moof boxes in data.  data is modified in-place.
func normaliseTfdt(data []byte, minTimes map[uint32]uint64) {
	pos := 0
	for pos+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[pos:]))
		if size < 8 || pos+size > len(data) {
			break
		}
		if string(data[pos+4:pos+8]) == "moof" {
			moofContent := data[pos+8 : pos+size]
			inner := 0
			for inner+8 <= len(moofContent) {
				innerSize := int(binary.BigEndian.Uint32(moofContent[inner:]))
				if innerSize < 8 || inner+innerSize > len(moofContent) {
					break
				}
				if string(moofContent[inner+4:inner+8]) == "traf" {
					normaliseTrafTfdt(moofContent[inner+8:inner+innerSize], minTimes)
				}
				inner += innerSize
			}
		}
		pos += size
	}
}


// extractMoofFirstTfdt returns the baseMediaDecodeTime from the first moof found
// in data (a raw fMP4 segment). Returns (0, false) if not found.
func extractMoofFirstTfdt(data []byte) (uint64, bool) {
	pos := 0
	for pos+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[pos:]))
		if size < 8 || pos+size > len(data) {
			break
		}
		if string(data[pos+4:pos+8]) == "moof" {
			_, dt, err := extractMoofInfo(data[pos+8 : pos+size])
			if err == nil {
				return dt, true
			}
		}
		pos += size
	}
	return 0, false
}

// shiftSegmentTfdt returns a copy of data with the tfdt.baseMediaDecodeTime for
// the given trackID decremented by base. Returns data unchanged if base is 0.
func shiftSegmentTfdt(data []byte, trackID uint32, base uint64) []byte {
	if base == 0 {
		return data
	}
	result := make([]byte, len(data))
	copy(result, data)
	normaliseTfdt(result, map[uint32]uint64{trackID: base})
	return result
}

// shiftSegmentAllTracks returns a copy of data with tfdt normalised for ALL
// tracks using the provided base time map. Use this for muxed fMP4 segments
// (e.g. Stripchat) where video and audio share a single segment file.
func shiftSegmentAllTracks(data []byte, baseTimes map[uint32]uint64) []byte {
	if len(baseTimes) == 0 {
		return data
	}
	result := make([]byte, len(data))
	copy(result, data)
	normaliseTfdt(result, baseTimes)
	return result
}

// extractAllTrackBaseTimes returns the tfdt base times for every traf in the
// first moof box. Returns nil if no moof/traf/tfdt is found.
func extractAllTrackBaseTimes(data []byte) map[uint32]uint64 {
	pos := 0
	for pos+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[pos:]))
		if size < 8 || pos+size > len(data) {
			break
		}
		if string(data[pos+4:pos+8]) == "moof" {
			return extractMoofTrackTimes(data[pos+8 : pos+size])
		}
		pos += size
	}
	return nil
}

// extractMoofTrackTimes iterates over all traf boxes inside a moof and returns
// a map of track_id -> baseMediaDecodeTime.
func extractMoofTrackTimes(moofContent []byte) map[uint32]uint64 {
	result := map[uint32]uint64{}
	inner := 0
	for inner+8 <= len(moofContent) {
		innerSize := int(binary.BigEndian.Uint32(moofContent[inner:]))
		if innerSize < 8 || inner+innerSize > len(moofContent) {
			break
		}
		if string(moofContent[inner+4:inner+8]) == "traf" {
			trafContent := moofContent[inner+8 : inner+innerSize]
			tid, dt := extractTrafTrackAndTfdt(trafContent)
			if tid != 0 {
				result[tid] = dt
			}
		}
		inner += innerSize
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// extractTrafTrackAndTfdt reads the track_id from tfhd and baseMediaDecodeTime
// from tfdt inside a single traf box.
func extractTrafTrackAndTfdt(trafContent []byte) (uint32, uint64) {
	var trackID uint32
	var decodeTime uint64
	pos := 0
	for pos+8 <= len(trafContent) {
		size := int(binary.BigEndian.Uint32(trafContent[pos:]))
		if size < 8 || pos+size > len(trafContent) {
			break
		}
		boxType := string(trafContent[pos+4 : pos+8])
		if boxType == "tfhd" && pos+16 <= len(trafContent) {
			trackID = binary.BigEndian.Uint32(trafContent[pos+12:])
		}
		if boxType == "tfdt" && size >= 16 {
			version := trafContent[pos+8]
			if version == 1 && size >= 20 {
				decodeTime = binary.BigEndian.Uint64(trafContent[pos+12:])
			} else {
				decodeTime = uint64(binary.BigEndian.Uint32(trafContent[pos+12:]))
			}
		}
		pos += size
	}
	return trackID, decodeTime
}

// normaliseTrafTfdt reads track_id from the tfhd box inside a traf, then
// subtracts minTimes[track_id] from the baseMediaDecodeTime in the tfdt box.
// trafContent is a slice of the parent data buffer, so edits are in-place.
func normaliseTrafTfdt(trafContent []byte, minTimes map[uint32]uint64) {
	// First pass: locate tfhd to identify the track.
	var trackID uint32
	pos := 0
	for pos+8 <= len(trafContent) {
		size := int(binary.BigEndian.Uint32(trafContent[pos:]))
		if size < 8 || pos+size > len(trafContent) {
			break
		}
		if string(trafContent[pos+4:pos+8]) == "tfhd" && pos+16 <= len(trafContent) {
			trackID = binary.BigEndian.Uint32(trafContent[pos+12:])
			break
		}
		pos += size
	}
	if trackID == 0 {
		return
	}
	minT := minTimes[trackID]
	if minT == 0 {
		return
	}

	// Second pass: patch the tfdt box.
	pos = 0
	for pos+8 <= len(trafContent) {
		size := int(binary.BigEndian.Uint32(trafContent[pos:]))
		if size < 8 || pos+size > len(trafContent) {
			break
		}
		if string(trafContent[pos+4:pos+8]) == "tfdt" && pos+9 <= len(trafContent) {
			version := trafContent[pos+8]
			if version == 1 && pos+20 <= len(trafContent) {
				cur := binary.BigEndian.Uint64(trafContent[pos+12:])
				if cur >= minT {
					binary.BigEndian.PutUint64(trafContent[pos+12:], cur-minT)
				}
			} else if version == 0 && pos+16 <= len(trafContent) {
				cur := uint64(binary.BigEndian.Uint32(trafContent[pos+12:]))
				if cur >= minT {
					binary.BigEndian.PutUint32(trafContent[pos+12:], uint32(cur-minT))
				}
			}
			return // only one tfdt per traf
		}
		pos += size
	}
}

// extractMoofInfo digs into a moof box's content (without the 8-byte header)
// and returns the track_id from tfhd and the baseMediaDecodeTime from tfdt.
func extractMoofInfo(moofContent []byte) (trackID uint32, decodeTime uint64, err error) {
	boxes, err := parseMP4Boxes(moofContent)
	if err != nil {
		return 0, 0, err
	}
	for _, b := range boxes {
		if b.typ != "traf" {
			continue
		}
		trafBoxes, _ := parseMP4Boxes(b.data[8:]) // skip traf header
		for _, tb := range trafBoxes {
			switch tb.typ {
			case "tfhd":
				// tfhd: [4 size][4 "tfhd"][1 version][3 flags][4 track_id]
				if len(tb.data) >= 16 {
					trackID = binary.BigEndian.Uint32(tb.data[12:])
				}
			case "tfdt":
				// tfdt: [4 size][4 "tfdt"][1 version][3 flags][4 or 8 baseMediaDecodeTime]
				if len(tb.data) >= 9 {
					version := tb.data[8]
					if version == 1 && len(tb.data) >= 20 {
						decodeTime = binary.BigEndian.Uint64(tb.data[12:])
					} else if version == 0 && len(tb.data) >= 16 {
						decodeTime = uint64(binary.BigEndian.Uint32(tb.data[12:]))
					}
				}
			}
		}
		if trackID != 0 {
			return trackID, decodeTime, nil
		}
	}
	return 0, 0, fmt.Errorf("tfhd not found in moof")
}

// buildMFRABox assembles the complete mfra box (header + tfra(s) + mfro).
func buildMFRABox(trackEntries map[uint32][]tfraEntry) []byte {
	// Sort track IDs for deterministic output (video track 1 before audio track 2).
	trackIDs := make([]uint32, 0, len(trackEntries))
	for id := range trackEntries {
		trackIDs = append(trackIDs, id)
	}
	sort.Slice(trackIDs, func(i, j int) bool { return trackIDs[i] < trackIDs[j] })

	// Build all tfra boxes.
	var tfraBytes []byte
	for _, id := range trackIDs {
		tfraBytes = append(tfraBytes, buildTFRA(id, trackEntries[id])...)
	}

	// mfra total size = 8 (mfra header) + len(tfraBytes) + 16 (mfro)
	mfraSize := uint32(8 + len(tfraBytes) + 16)
	mfro := buildMFRO(mfraSize)

	content := append(tfraBytes, mfro...)
	return makeMP4Box("mfra", content)
}

// buildTFRA builds a tfra (Track Fragment Random Access) box for one track.
// Uses version=1 (64-bit time and offset) and 1-byte traf/trun/sample fields.
func buildTFRA(trackID uint32, entries []tfraEntry) []byte {
	// Per-entry size: 8 (time) + 8 (offset) + 1 (traf) + 1 (trun) + 1 (sample) = 19 bytes
	const entrySize = 19
	// Box layout:
	//   [4 size][4 "tfra"][1 version=1][3 flags=0]
	//   [4 track_ID][4 reserved+length_sizes][4 number_of_entry]
	//   entries x [8 time][8 moof_offset][1 traf_number][1 trun_number][1 sample_number]
	boxSize := 24 + len(entries)*entrySize
	b := make([]byte, boxSize)

	binary.BigEndian.PutUint32(b[0:], uint32(boxSize))
	copy(b[4:], "tfra")
	b[8] = 1 // version = 1 (64-bit time and offset)
	// b[9:12]  flags = 0
	binary.BigEndian.PutUint32(b[12:], trackID)
	// b[16:20] reserved(26) + length_size_of_traf_num(2) + length_size_of_trun_num(2) + length_size_of_sample_num(2) = 0
	binary.BigEndian.PutUint32(b[20:], uint32(len(entries)))

	off := 24
	for _, e := range entries {
		binary.BigEndian.PutUint64(b[off:], e.time)
		binary.BigEndian.PutUint64(b[off+8:], e.offset)
		b[off+16] = 1 // traf_number = 1
		b[off+17] = 1 // trun_number = 1
		b[off+18] = 1 // sample_number = 1
		off += entrySize
	}
	return b
}

// buildMFRO builds the mfro (Movie Fragment Random Access Offset) box.
// mfraSize must be the total byte count of the enclosing mfra box.
func buildMFRO(mfraSize uint32) []byte {
	// [4 size=16][4 "mfro"][1 version=0][3 flags=0][4 mfra_size]
	b := make([]byte, 16)
	binary.BigEndian.PutUint32(b[0:], 16)
	copy(b[4:], "mfro")
	// b[8:12] version+flags = 0
	binary.BigEndian.PutUint32(b[12:], mfraSize)
	return b
}
