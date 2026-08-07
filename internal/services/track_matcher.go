// Governing: SPEC playlist-sync-navidrome REQ-PLSYNC-001 through REQ-PLSYNC-005 (track matching strategies)
package services

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"spotter/ent"
	"spotter/ent/artist"
	"spotter/ent/track"
	"spotter/ent/user"
	"spotter/internal/providers"
)

// MatchMethod describes how a track was matched.
type MatchMethod string

const (
	MatchMethodNone  MatchMethod = "none"
	MatchMethodISRC  MatchMethod = "isrc"
	MatchMethodExact MatchMethod = "exact"
	MatchMethodFuzzy MatchMethod = "fuzzy"
)

// MatchResult contains the result of matching a source track to the local library.
type MatchResult struct {
	SourceTrack      providers.Track
	NavidromeTrackID string      // Empty if no match found
	MatchConfidence  float64     // 0.0 to 1.0
	MatchMethod      MatchMethod // "exact", "fuzzy", "isrc", "none"
}

// TrackMatcher matches source tracks from external providers to local Navidrome tracks.
type TrackMatcher struct {
	Client             *ent.Client
	Logger             *slog.Logger
	MinMatchConfidence float64 // Minimum confidence for fuzzy matching (0.0-1.0)
}

// NewTrackMatcher creates a new TrackMatcher with the specified minimum match confidence.
func NewTrackMatcher(client *ent.Client, logger *slog.Logger, minConfidence float64) *TrackMatcher {
	return &TrackMatcher{
		Client:             client,
		Logger:             logger,
		MinMatchConfidence: minConfidence,
	}
}

// NormalizedCandidate is a library track whose match keys have been normalized
// once, up front. Fuzzy matching compares against the precomputed rune slices
// so per-source-track work never re-runs normalizeForMatch over the library.
// Exported so other fuzzy-matching consumers (e.g. the vibes matcher, story
// #340) can reuse the same candidate representation.
type NormalizedCandidate struct {
	Track       *ent.Track
	TitleRunes  []rune // normalizeForMatch(track name), in runes
	ArtistRunes []rune // normalizeForMatch(artist name), in runes
}

// LibraryIndex is a precomputed matching index over a user's Navidrome library.
// Build it once per sync tick with LoadLibraryIndex and reuse it across
// playlists via MatchTracksWithIndex so matching cost no longer scales with
// playlist count x library size.
type LibraryIndex struct {
	UserID     int
	isrcMap    map[string]*ent.Track // key: lowercased ISRC
	exactMap   map[string]*ent.Track // key: normalized "artist|title"
	candidates []NormalizedCandidate
}

// Size returns the number of library tracks in the index.
func (idx *LibraryIndex) Size() int {
	return len(idx.candidates)
}

// LoadLibraryIndex loads the user's Navidrome-linked library once and builds
// the lookup maps and normalized fuzzy-match candidates.
// Governing: ADR-0014 (lookup maps for tiers 1-2, normalized candidates for tier 3)
func (m *TrackMatcher) LoadLibraryIndex(ctx context.Context, userID int) (*LibraryIndex, error) {
	// Get all tracks for this user's library (tracks that have a navidrome_id)
	// FIX: Filter by user through the artist edge: Track -> Artist -> User
	// The explicit ordering makes the index deterministic: without it, which
	// track wins a duplicate exactMap/isrcMap key (or a fuzzy-score tie) is
	// DB-planner-dependent and can flip between ticks, churning the synced
	// Navidrome playlists.
	libraryTracks, err := m.Client.Track.Query().
		Where(
			track.NavidromeIDNotNil(),
			track.HasArtistWith(artist.HasUserWith(user.ID(userID))),
		).
		WithArtist().
		Order(ent.Asc(track.FieldID)).
		All(ctx)
	if err != nil {
		m.Logger.Error("failed to query library tracks",
			"user_id", userID,
			"error", err)
		return nil, err
	}

	idx := &LibraryIndex{
		UserID:     userID,
		isrcMap:    make(map[string]*ent.Track),
		exactMap:   make(map[string]*ent.Track),
		candidates: make([]NormalizedCandidate, 0, len(libraryTracks)),
	}

	for _, t := range libraryTracks {
		// ISRC map (if available)
		if t.Isrc != nil && *t.Isrc != "" {
			isrcKey := strings.ToLower(*t.Isrc)
			idx.isrcMap[isrcKey] = t
		}

		artistName := ""
		if t.Edges.Artist != nil {
			artistName = t.Edges.Artist.Name
		}
		normalizedTitle := normalizeTitleForMatch(t.Name)
		normalizedArtist := normalizeForMatch(artistName)

		// Exact match map
		key := normalizedArtist + "|" + normalizedTitle
		idx.exactMap[key] = t

		// Fuzzy match candidate with precomputed normalized runes.
		// The query above already filters NavidromeIDNotNil, so every track
		// here is a valid candidate.
		idx.candidates = append(idx.candidates, NormalizedCandidate{
			Track:       t,
			TitleRunes:  []rune(normalizedTitle),
			ArtistRunes: []rune(normalizedArtist),
		})
	}

	m.Logger.Debug("built library index",
		"user_id", userID,
		"library_track_count", len(libraryTracks),
		"isrc_map_size", len(idx.isrcMap),
		"exact_map_size", len(idx.exactMap))

	return idx, nil
}

// MatchTracks attempts to find Navidrome track IDs for source tracks.
// Uses multiple strategies: ISRC matching, exact name match, fuzzy matching.
// It loads the library index on every call; callers matching multiple
// playlists in one tick should use LoadLibraryIndex + MatchTracksWithIndex.
func (m *TrackMatcher) MatchTracks(ctx context.Context, userID int, tracks []providers.Track) ([]MatchResult, error) {
	idx, err := m.LoadLibraryIndex(ctx, userID)
	if err != nil {
		return nil, err
	}
	return m.MatchTracksWithIndex(idx, tracks)
}

// MatchTracksWithIndex matches source tracks against a precomputed LibraryIndex.
// It performs no I/O: the index must have been built via LoadLibraryIndex, and
// passing a nil index is an error.
// Governing: ADR-0014 (three-tier ISRC -> exact -> fuzzy matching)
func (m *TrackMatcher) MatchTracksWithIndex(idx *LibraryIndex, tracks []providers.Track) ([]MatchResult, error) {
	if idx == nil {
		return nil, fmt.Errorf("track matcher: nil LibraryIndex; call LoadLibraryIndex first")
	}

	startTime := time.Now()
	results := make([]MatchResult, len(tracks))

	m.Logger.Info("starting track matching",
		"user_id", idx.UserID,
		"source_track_count", len(tracks),
		"library_track_count", idx.Size(),
		"min_match_confidence", m.MinMatchConfidence)

	if idx.Size() == 0 {
		// Not an early return: the loop below already yields the same
		// all-unmatched results for an empty index, and running it keeps the
		// per-track REQ MATCH-003 metric.track_match emissions and summary log.
		m.Logger.Warn("no tracks with navidrome_id found for user",
			"user_id", idx.UserID,
			"hint", "ensure Navidrome sync has run to populate navidrome_id on tracks")
	}

	// Track match statistics by method
	matchStats := map[MatchMethod]int{
		MatchMethodNone:  0,
		MatchMethodISRC:  0,
		MatchMethodExact: 0,
		MatchMethodFuzzy: 0,
	}

	// Governing: ADR-0019 (structured metrics), SPEC observability REQ "MATCH-001", REQ "MATCH-002", REQ "MATCH-003"
	// Match each source track
	for i, sourceTrack := range tracks {
		trackMatchStart := time.Now()
		result := MatchResult{
			SourceTrack:     sourceTrack,
			MatchConfidence: 0.0,
			MatchMethod:     MatchMethodNone,
		}

		// Strategy 1: ISRC matching (highest confidence)
		if sourceTrack.ISRC != "" {
			isrcKey := strings.ToLower(sourceTrack.ISRC)
			if matchedTrack, ok := idx.isrcMap[isrcKey]; ok {
				if matchedTrack.NavidromeID != nil {
					result.NavidromeTrackID = *matchedTrack.NavidromeID
					result.MatchConfidence = 1.0
					result.MatchMethod = MatchMethodISRC
					matchStats[MatchMethodISRC]++
					results[i] = result

					m.Logger.Debug("matched track by ISRC",
						"source_artist", sourceTrack.Artist,
						"source_title", sourceTrack.Name,
						"isrc", sourceTrack.ISRC,
						"navidrome_id", *matchedTrack.NavidromeID)
					// REQ "MATCH-002": emit metric for the successful strategy only
					m.Logger.Info("metric.track_match",
						"strategy", "isrc",
						"matched", true,
						"confidence", 1.0,
						"duration_ms", time.Since(trackMatchStart).Milliseconds())
					continue
				}
			}
		}

		// Strategy 2: Exact match (artist + title)
		normalizedTitle := normalizeTitleForMatch(sourceTrack.Name)
		normalizedArtist := normalizeForMatch(sourceTrack.Artist)
		exactKey := normalizedArtist + "|" + normalizedTitle
		if matchedTrack, ok := idx.exactMap[exactKey]; ok {
			if matchedTrack.NavidromeID != nil {
				result.NavidromeTrackID = *matchedTrack.NavidromeID
				result.MatchConfidence = 1.0
				result.MatchMethod = MatchMethodExact
				matchStats[MatchMethodExact]++
				results[i] = result

				m.Logger.Debug("matched track by exact match",
					"source_artist", sourceTrack.Artist,
					"source_title", sourceTrack.Name,
					"navidrome_id", *matchedTrack.NavidromeID)
				// REQ "MATCH-002": emit metric for the successful strategy only
				m.Logger.Info("metric.track_match",
					"strategy", "exact",
					"matched", true,
					"confidence", 1.0,
					"duration_ms", time.Since(trackMatchStart).Milliseconds())
				continue
			}
		}

		// Strategy 3: Fuzzy matching
		bestMatch, confidence := findBestFuzzyMatch([]rune(normalizedTitle), []rune(normalizedArtist), idx.candidates)
		if bestMatch != nil && confidence >= m.MinMatchConfidence {
			if bestMatch.NavidromeID != nil {
				result.NavidromeTrackID = *bestMatch.NavidromeID
				result.MatchConfidence = confidence
				result.MatchMethod = MatchMethodFuzzy
				matchStats[MatchMethodFuzzy]++

				matchedArtist := ""
				if bestMatch.Edges.Artist != nil {
					matchedArtist = bestMatch.Edges.Artist.Name
				}

				m.Logger.Debug("matched track by fuzzy match",
					"source_artist", sourceTrack.Artist,
					"source_title", sourceTrack.Name,
					"matched_artist", matchedArtist,
					"matched_title", bestMatch.Name,
					"confidence", confidence,
					"navidrome_id", *bestMatch.NavidromeID)
				// REQ "MATCH-002": emit metric for the successful strategy only
				m.Logger.Info("metric.track_match",
					"strategy", "fuzzy",
					"matched", true,
					"confidence", confidence,
					"duration_ms", time.Since(trackMatchStart).Milliseconds())
			}
		} else {
			matchStats[MatchMethodNone]++

			m.Logger.Debug("no match found for track",
				"source_artist", sourceTrack.Artist,
				"source_title", sourceTrack.Name,
				"best_confidence", confidence,
				"min_required", m.MinMatchConfidence)
			// REQ "MATCH-003": all strategies failed, emit strategy="fuzzy", matched=false, confidence=0.0
			m.Logger.Info("metric.track_match",
				"strategy", "fuzzy",
				"matched", false,
				"confidence", 0.0,
				"duration_ms", time.Since(trackMatchStart).Milliseconds())
		}

		results[i] = result
	}

	// Log summary
	matched := 0
	for _, r := range results {
		if r.NavidromeTrackID != "" {
			matched++
		}
	}

	// Guard the divide: an empty playlist would otherwise log match_rate=NaN,
	// which the JSON slog handler emits as an !ERROR placeholder.
	matchRate := 0.0
	if len(tracks) > 0 {
		matchRate = float64(matched) / float64(len(tracks)) * 100
	}

	duration := time.Since(startTime)
	m.Logger.Info("track matching complete",
		"user_id", idx.UserID,
		"total_tracks", len(tracks),
		"matched_tracks", matched,
		"unmatched_tracks", len(tracks)-matched,
		"match_rate", matchRate,
		"matches_by_isrc", matchStats[MatchMethodISRC],
		"matches_by_exact", matchStats[MatchMethodExact],
		"matches_by_fuzzy", matchStats[MatchMethodFuzzy],
		"duration_ms", duration.Milliseconds())

	return results, nil
}

// findBestFuzzyMatch finds the best fuzzy match for a normalized source
// title/artist among precomputed candidates.
// Governing: SPEC track-matching REQ-TM-031, REQ-TM-032 (weighted score + dual-confidence bonus)
func findBestFuzzyMatch(sourceTitle, sourceArtist []rune, candidates []NormalizedCandidate) (*ent.Track, float64) {
	var bestMatch *ent.Track
	bestScore := 0.0

	for _, candidate := range candidates {
		titleScore := similarity(sourceTitle, candidate.TitleRunes)

		// Upper bound for this candidate: artistScore <= 1.0, and the
		// dual-confidence bonus (+0.1) only applies when titleScore > 0.8.
		// Candidates that provably cannot beat the current best skip the
		// second Levenshtein pass entirely.
		maxPossible := titleScore*0.6 + 0.4
		if titleScore > 0.8 {
			maxPossible += 0.1
		}
		if maxPossible <= bestScore {
			continue
		}

		artistScore := similarity(sourceArtist, candidate.ArtistRunes)

		// Weighted average: title is more important
		score := (titleScore * 0.6) + (artistScore * 0.4)

		// Bonus for both being high confidence
		if titleScore > 0.8 && artistScore > 0.8 {
			score += 0.1
			if score > 1.0 {
				score = 1.0
			}
		}

		if score > bestScore {
			bestScore = score
			bestMatch = candidate.Track
			// Nothing can beat a perfect score (the comparison is strict >).
			if bestScore >= 1.0 {
				break
			}
		}
	}

	return bestMatch, bestScore
}

// yearVersionSuffixRe strips year-qualified remaster suffixes that the literal
// suffix list below cannot match with strings.TrimSuffix: " - remastered 2011",
// "- 2011 remaster", "(remastered 2011)", "[2009 remaster]".
// Governing: SPEC track-matching REQ-TM-041 (scenario 3: "Don't Stop Me Now - Remastered 2011")
var yearVersionSuffixRe = regexp.MustCompile(`\s*(?:-\s*|[(\[])\s*(?:\d{4}\s+)?remaster(?:ed)?(?:\s+\d{4})?\s*[)\]]?\s*$`)

// normalizeForMatch normalizes a string for comparison.
func normalizeForMatch(s string) string {
	// Canonicalize to NFC first so NFC- and NFD-encoded metadata (e.g.
	// macOS-ripped tags with decomposed Hangul or combining accents) normalize
	// identically; without this, NFC "한" (1 rune) vs NFD "한" (3 jamo runes)
	// misses the exact tier and scores 0.0 in the fuzzy tier.
	// Governing: SPEC track-matching REQ-TM-042 (unicode letters/numbers preserved)
	s = norm.NFC.String(s)

	// Convert to lowercase
	s = strings.ToLower(s)

	// Strip year-qualified remaster suffixes before the literal suffix list.
	s = yearVersionSuffixRe.ReplaceAllString(s, "")

	// Remove common suffixes that indicate versions
	suffixes := []string{
		"(remastered)",
		"(remaster)",
		"(deluxe)",
		"(deluxe edition)",
		"(bonus track)",
		"(bonus tracks)",
		"(radio edit)",
		"(single version)",
		"(album version)",
		"(explicit)",
		"(clean)",
		"(live)",
		"(acoustic)",
		"(remix)",
		"[remastered]",
		"[remaster]",
		"[deluxe]",
		"[deluxe edition]",
		"[bonus track]",
		"[bonus tracks]",
		"[radio edit]",
		"[single version]",
		"[album version]",
		"[explicit]",
		"[clean]",
		"[live]",
		"[acoustic]",
		"[remix]",
		" - remastered",
		" - remaster",
		" - deluxe",
		" - live",
		" - acoustic",
	}

	for _, suffix := range suffixes {
		s = strings.TrimSuffix(s, suffix)
	}

	// Remove punctuation and extra whitespace
	var result strings.Builder
	lastWasSpace := false

	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			result.WriteRune(r)
			lastWasSpace = false
		} else if unicode.IsSpace(r) && !lastWasSpace {
			result.WriteRune(' ')
			lastWasSpace = true
		}
	}

	return strings.TrimSpace(result.String())
}

// normalizeTitleForMatch normalizes a title for matching, falling back to the
// lowercased raw string when normalization strips every rune (symbol-only
// titles such as "!!!" or "†††"). Without the fallback all such titles
// collapse to "" and match each other at confidence 1.0 — via colliding
// exactMap keys in tier 2 and the both-empty similarity fast path in tier 3.
func normalizeTitleForMatch(s string) string {
	if n := normalizeForMatch(s); n != "" {
		return n
	}
	return strings.ToLower(strings.TrimSpace(norm.NFC.String(s)))
}

// similarity calculates the similarity between two rune slices using
// Levenshtein distance. Returns a value between 0.0 (completely different)
// and 1.0 (identical).
//
// Both the edit distance and the maximum length are measured in RUNES.
// Mixing a rune-based distance with a byte-based max length inflated
// similarity 2-3x for non-ASCII (CJK/Cyrillic) titles, letting wrong tracks
// clear the fuzzy threshold.
// Governing: SPEC track-matching REQ-TM-031 (issue #330: distance and max length in rune units)
func similarity(a, b []rune) float64 {
	// Fast path: identical strings (including both-empty) skip the DP matrix.
	if slices.Equal(a, b) {
		return 1.0
	}
	if len(a) == 0 || len(b) == 0 {
		return 0.0
	}

	// Calculate Levenshtein distance (in runes)
	distance := levenshtein(a, b)
	maxLen := max(len(a), len(b))

	return 1.0 - float64(distance)/float64(maxLen)
}

// levenshtein calculates the Levenshtein distance between two rune slices,
// measured in rune edits.
func levenshtein(aRunes, bRunes []rune) int {
	if len(aRunes) == 0 {
		return len(bRunes)
	}
	if len(bRunes) == 0 {
		return len(aRunes)
	}

	// Two rolling rows instead of a full DP matrix: levenshtein sits on the
	// O(source tracks x library candidates) fuzzy-match hot path, and the full
	// matrix cost len(a)+1 heap allocations per call.
	prev := make([]int, len(bRunes)+1)
	curr := make([]int, len(bRunes)+1)

	// Initialize first row
	for j := 0; j <= len(bRunes); j++ {
		prev[j] = j
	}

	// Fill in the rest
	for i := 1; i <= len(aRunes); i++ {
		curr[0] = i
		for j := 1; j <= len(bRunes); j++ {
			cost := 1
			if aRunes[i-1] == bRunes[j-1] {
				cost = 0
			}

			curr[j] = min(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}

	return prev[len(bRunes)]
}

// GetUnmatchedTracks returns tracks that couldn't be matched to the library.
func GetUnmatchedTracks(results []MatchResult) []MatchResult {
	var unmatched []MatchResult
	for _, r := range results {
		if r.NavidromeTrackID == "" {
			unmatched = append(unmatched, r)
		}
	}
	return unmatched
}

// GetMatchedTracks returns tracks that were successfully matched.
func GetMatchedTracks(results []MatchResult) []MatchResult {
	var matched []MatchResult
	for _, r := range results {
		if r.NavidromeTrackID != "" {
			matched = append(matched, r)
		}
	}
	return matched
}

// MatchStats contains statistics about a matching operation.
type MatchStats struct {
	Total         int
	Matched       int
	Unmatched     int
	ByMethod      map[MatchMethod]int
	AvgConfidence float64
}

// GetMatchStats calculates statistics for a set of match results.
func GetMatchStats(results []MatchResult) MatchStats {
	stats := MatchStats{
		Total:    len(results),
		ByMethod: make(map[MatchMethod]int),
	}

	totalConfidence := 0.0
	matchedCount := 0

	for _, r := range results {
		stats.ByMethod[r.MatchMethod]++
		if r.NavidromeTrackID != "" {
			stats.Matched++
			totalConfidence += r.MatchConfidence
			matchedCount++
		} else {
			stats.Unmatched++
		}
	}

	if matchedCount > 0 {
		stats.AvgConfidence = totalConfidence / float64(matchedCount)
	}

	return stats
}
