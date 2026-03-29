package stripchat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/HeapOfChaos/goondvr/internal"
	"github.com/HeapOfChaos/goondvr/server"
)

// knownPDKeys maps pkey (from #EXT-X-MOUFLON:PSCH:v2:{pkey}) to pdkey (decryption key).
// The real pdkey is NOT in the visible JS ie object (those are decoys).
// It must be found via Chrome DevTools breakpoint on _onPlaylistLoadingStateChanged
// in the Closure scope (Ct object). See plugin.video.sc19#19.
var knownPDKeys = map[string]string{
	"Ook7quaiNgiyuhai": "EQueeGh2kaewa3ch",
}

var (
	pdkeyMu       sync.Mutex
	verifiedPDKey  string   // confirmed working pdkey (produces printable ASCII)
	candidateKeys  []string // 16-char alphanumeric strings extracted from player JS
	pdkeyFetched   bool
)

// ResolvePDKey returns the MOUFLON v2 decryption key for the given pkey.
// Priority: manual override → verified key → triggers auto-extraction (candidates
// will be tested against a live token later via TryFindWorkingKey).
func ResolvePDKey(ctx context.Context, pkey string) string {
	// Manual override always wins.
	if server.Config.StripchatPDKey != "" {
		return server.Config.StripchatPDKey
	}

	pdkeyMu.Lock()
	defer pdkeyMu.Unlock()

	if verifiedPDKey != "" {
		return verifiedPDKey
	}

	// Check hardcoded known keys.
	if pdkey, ok := knownPDKeys[pkey]; ok {
		return pdkey
	}

	// Auto-extract candidate keys from the mmp player JS (once).
	if !pdkeyFetched {
		pdkeyFetched = true
		candidates, err := fetchCandidateKeysFromPlayer(ctx)
		if err != nil {
			fmt.Printf("[stripchat] WARNING: auto-extract MOUFLON keys failed: %v\n", err)
			fmt.Println("[stripchat] Use --stripchat-pdkey to set the decryption key manually.")
		} else {
			candidateKeys = candidates
			if server.Config.Debug {
				fmt.Printf("[DEBUG] mouflon: extracted %d candidate keys from player JS\n", len(candidates))
			}
		}
	}

	// Return "pending" to signal that decodeMouflon should call TryFindWorkingKey.
	if len(candidateKeys) > 0 {
		return "pending"
	}
	return ""
}

// TryFindWorkingKey tests all candidate keys against a sample MOUFLON-encrypted
// URI. Returns the verified pdkey, or empty string if none produce valid output.
// This is called from decodeMouflon on the first encrypted segment.
func TryFindWorkingKey(sampleURI string) string {
	pdkeyMu.Lock()
	defer pdkeyMu.Unlock()

	if verifiedPDKey != "" {
		return verifiedPDKey
	}
	if server.Config.StripchatPDKey != "" {
		return server.Config.StripchatPDKey
	}

	if server.Config.Debug {
		fmt.Printf("[DEBUG] mouflon: testing %d candidate keys against sample URI\n", len(candidateKeys))
	}

	for _, key := range candidateKeys {
		result, err := decryptToken(sampleURI, key)
		if err != nil {
			continue
		}
		if isPrintableASCII(result) {
			verifiedPDKey = key
			fmt.Printf("[stripchat] MOUFLON: found working pdkey (%d chars) by testing %d candidates\n", len(key), len(candidateKeys))
			if server.Config.Debug {
				fmt.Printf("[DEBUG] mouflon: verified pdkey=%q decrypted sample=%q\n", key, string(result))
			}
			return key
		}
		if server.Config.Debug {
			fmt.Printf("[DEBUG] mouflon: candidate %q → non-printable (hex=%x)\n", key, result)
		}
	}

	if len(candidateKeys) > 0 {
		fmt.Printf("[stripchat] WARNING: none of %d candidate keys produced valid decryption\n", len(candidateKeys))
	}
	fmt.Println("[stripchat] Set the decryption key manually with --stripchat-pdkey.")
	fmt.Println("[stripchat] To find the key, see: https://github.com/aitschti/plugin.video.sc19/issues/19")
	return ""
}

// ResetPDKeyCache clears all cached keys so the next call will re-attempt extraction.
func ResetPDKeyCache() {
	pdkeyMu.Lock()
	defer pdkeyMu.Unlock()
	verifiedPDKey = ""
	candidateKeys = nil
	pdkeyFetched = false
}

// ParsePKeyFromMaster extracts the pkey from a master playlist's
// #EXT-X-MOUFLON:PSCH:v2:{pkey} line. Returns empty string if not found.
func ParsePKeyFromMaster(masterBody string) string {
	for _, line := range strings.Split(masterBody, "\n") {
		line = strings.TrimRight(line, "\r\n ")
		if strings.HasPrefix(line, "#EXT-X-MOUFLON:PSCH:") {
			// Format: #EXT-X-MOUFLON:PSCH:v2:{pkey}
			parts := strings.SplitN(line, ":", 4)
			if len(parts) == 4 {
				return parts[3]
			}
		}
	}
	return ""
}

// reToken matches _NUMBER_TOKEN_NUMBER patterns in segment URIs.
// The token (group 2) is the encrypted portion sandwiched between two numeric fields.
var reToken = regexp.MustCompile(`_(\d+)_([^_]+)_(\d+)`)

// DecryptMouflonURI decrypts the encrypted token in a MOUFLON v2 segment URI.
// Algorithm: reverse token -> base64-decode -> XOR with cyclic SHA256(pdkey).
// Returns an error if the decrypted result contains non-printable bytes.
func DecryptMouflonURI(uri, pdkey string) (string, error) {
	m := reToken.FindStringSubmatch(uri)
	if m == nil {
		return uri, nil
	}
	encryptedPart := m[2]

	result, err := decryptToken(uri, pdkey)
	if err != nil {
		return "", err
	}

	if !isPrintableASCII(result) {
		return "", fmt.Errorf("decryption produced non-printable bytes (hex=%x); pdkey is likely wrong", result)
	}

	decryptedPart := string(result)

	if server.Config.Debug {
		fmt.Printf("[DEBUG] mouflon decrypt: %q → %q\n", encryptedPart, decryptedPart)
	}

	// Replace encrypted token with decrypted value in the URI.
	decryptedURI := strings.Replace(uri, encryptedPart, decryptedPart, 1)
	return decryptedURI, nil
}

// decryptToken extracts and decrypts the encrypted token from a URI.
// Returns the raw decrypted bytes.
func decryptToken(uri, pdkey string) ([]byte, error) {
	m := reToken.FindStringSubmatch(uri)
	if m == nil {
		return nil, fmt.Errorf("no encrypted token found in URI")
	}
	encryptedPart := m[2]

	// Reverse the encrypted string.
	runes := []rune(encryptedPart)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	reversed := string(runes)

	// Base64 decode with padding.
	decoded, err := base64.StdEncoding.DecodeString(padBase64(reversed))
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(padBase64(reversed))
		if err != nil {
			return nil, fmt.Errorf("base64 decode %q: %w", reversed, err)
		}
	}

	// XOR with cyclic SHA256(pdkey).
	hash := sha256.Sum256([]byte(pdkey))
	result := make([]byte, len(decoded))
	for i, b := range decoded {
		result[i] = b ^ hash[i%32]
	}
	return result, nil
}

// isPrintableASCII returns true if all bytes are printable ASCII (space through tilde).
func isPrintableASCII(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}

// padBase64 adds "=" padding to make a base64 string length a multiple of 4.
func padBase64(s string) string {
	switch len(s) % 4 {
	case 2:
		return s + "=="
	case 3:
		return s + "="
	default:
		return s
	}
}

// fetchCandidateKeysFromPlayer fetches the Stripchat homepage, finds the mmp player
// JS chunk that contains MOUFLON code, and extracts ALL 16-character alphanumeric
// strings as candidate decryption keys. The correct pdkey is hidden among these
// candidates (Stripchat places decoy keys in visible objects to mislead scrapers).
func fetchCandidateKeysFromPlayer(ctx context.Context) ([]string, error) {
	// Use a media-style request (no X-Requested-With) to avoid 406 from Stripchat homepage.
	req := internal.NewMediaReqWithReferer("https://stripchat.com/")

	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pageBody, err := req.Get(ctx2, "https://stripchat.com/")
	if err != nil {
		return nil, fmt.Errorf("fetch stripchat homepage: %w", err)
	}

	// Find the mmp player base URL.
	var baseURL string
	reBase := regexp.MustCompile(`https://mmp\.doppiocdn\.com/player/mmp/v[0-9.]+/`)
	if m := reBase.FindString(pageBody); m != "" {
		baseURL = m
	}
	reOrigin := regexp.MustCompile(`(?:mmp|doppio)PlayerExternalSourceOrigin['":\s]+"(https://[^"]+)"`)
	if m := reOrigin.FindStringSubmatch(pageBody); len(m) > 1 {
		baseURL = strings.TrimRight(m[1], "/") + "/"
	}
	if baseURL == "" {
		return nil, fmt.Errorf("could not find mmp player base URL in Stripchat page")
	}

	if server.Config.Debug {
		fmt.Printf("[DEBUG] mouflon: player base URL: %s\n", baseURL)
	}

	// Find chunk JS URLs.
	reChunk := regexp.MustCompile(`chunk-[0-9a-f]{16,}\.js`)
	chunkNames := reChunk.FindAllString(pageBody, -1)
	seen := map[string]bool{}
	var uniqueChunks []string
	for _, c := range chunkNames {
		if !seen[c] {
			seen[c] = true
			uniqueChunks = append(uniqueChunks, c)
		}
	}

	if server.Config.Debug {
		fmt.Printf("[DEBUG] mouflon: found %d chunk URLs\n", len(uniqueChunks))
	}

	// Fetch each chunk; find the one with MOUFLON code and extract candidates.
	for _, chunkName := range uniqueChunks {
		chunkURL := baseURL + chunkName
		ctx3, cancel2 := context.WithTimeout(ctx, 15*time.Second)
		jsBody, err := req.Get(ctx3, chunkURL)
		cancel2()
		if err != nil {
			continue
		}
		if !strings.Contains(jsBody, "MOUFLON") {
			continue
		}

		if server.Config.Debug {
			fmt.Printf("[DEBUG] mouflon: chunk %s contains MOUFLON code (%d bytes)\n", chunkName, len(jsBody))
		}

		candidates := extractCandidateStrings(jsBody)
		if len(candidates) > 0 {
			return candidates, nil
		}
	}

	return nil, fmt.Errorf("no MOUFLON chunk found or no candidate keys extracted")
}

// re16Alphanum matches standalone 16-character alphanumeric strings (quoted in JS).
var re16Alphanum = regexp.MustCompile(`"([a-zA-Z0-9]{16})"`)

// extractCandidateStrings finds all unique 16-character alphanumeric strings in JS code.
// These are potential pdkeys — the correct one is verified at runtime by testing
// decryption against a real encrypted token.
func extractCandidateStrings(js string) []string {
	matches := re16Alphanum.FindAllStringSubmatch(js, -1)
	seen := map[string]bool{}
	var candidates []string
	for _, m := range matches {
		s := m[1]
		if seen[s] {
			continue
		}
		seen[s] = true
		// Skip strings that look like hex hashes (all lowercase hex chars).
		if isHexOnly(s) {
			continue
		}
		candidates = append(candidates, s)
	}
	return candidates
}

// isHexOnly returns true if the string contains only hex characters (0-9a-f).
func isHexOnly(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// mapKeys returns the keys of a string map for debug logging.
func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
