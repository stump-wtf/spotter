package services

import (
	"testing"

	"spotter/ent"
)

// Governing: SPEC track-matching REQ-TM-031 (similarity measured in rune units)
// Issue #330: similarity() previously divided a rune-based Levenshtein
// distance by a byte-based max length, inflating scores 2-3x for non-ASCII
// titles. These tests pin the rune-based behavior.

func runeSim(a, b string) float64 {
	return similarity([]rune(a), []rune(b))
}

func TestSimilarity_Regression_CJKDissimilarTitlesScoreZero(t *testing.T) {
	// "夜に駆ける" (5 runes) vs "群青" (2 runes): every rune differs.
	// Rune-based: distance 5 / maxLen 5 = similarity 0.0.
	// Byte-based maxLen (15 bytes) would have inflated this to ~0.67.
	score := runeSim("夜に駆ける", "群青")
	if score > 0.05 {
		t.Fatalf("expected ~0.0 similarity for dissimilar CJK titles, got %f", score)
	}

	// "君の名は" vs "千本桜": no runes in common.
	score = runeSim("君の名は", "千本桜")
	if score > 0.05 {
		t.Fatalf("expected ~0.0 similarity for dissimilar CJK titles, got %f", score)
	}
}

func TestSimilarity_Regression_CyrillicDissimilarTitlesScoreLow(t *testing.T) {
	// "калинка" vs "катюша": shares only leading "ка" and trailing "а".
	// Rune-based maxLen is 7; byte-based would have been 14, halving the
	// apparent distance ratio.
	score := runeSim("калинка", "катюша")
	if score > 0.45 {
		t.Fatalf("expected low similarity for dissimilar Cyrillic titles, got %f", score)
	}

	// Fully dissimilar Cyrillic strings should score ~0.0.
	score = runeSim("жизнь", "туман")
	if score > 0.05 {
		t.Fatalf("expected ~0.0 similarity for dissimilar Cyrillic titles, got %f", score)
	}
}

func TestSimilarity_Identical(t *testing.T) {
	for _, s := range []string{"hello", "夜に駆ける", "калинка", ""} {
		if got := runeSim(s, s); got != 1.0 {
			t.Fatalf("expected 1.0 for identical strings %q, got %f", s, got)
		}
	}
}

func TestSimilarity_Empty(t *testing.T) {
	if got := runeSim("", "abc"); got != 0.0 {
		t.Fatalf("expected 0.0 for empty vs non-empty, got %f", got)
	}
	if got := runeSim("abc", ""); got != 0.0 {
		t.Fatalf("expected 0.0 for non-empty vs empty, got %f", got)
	}
}

func TestSimilarity_ASCII_UnchangedByRuneFix(t *testing.T) {
	// ASCII behavior must be identical to the previous implementation
	// (bytes == runes for ASCII): "kitten" vs "sitting" has distance 3,
	// maxLen 7 -> 1 - 3/7.
	got := runeSim("kitten", "sitting")
	want := 1.0 - 3.0/7.0
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("expected %f for kitten/sitting, got %f", want, got)
	}
}

func TestLevenshtein_RuneUnits(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"夜に駆ける", "群青", 5},    // all 5 runes replaced/deleted
		{"君の名は", "君の名は", 0},   // identical CJK
		{"кошка", "мошка", 1}, // single rune substitution
	}
	for _, c := range cases {
		if got := levenshtein([]rune(c.a), []rune(c.b)); got != c.want {
			t.Fatalf("levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFindBestFuzzyMatch_SuffixVariantsStillMatch(t *testing.T) {
	// Known remaster/suffix variants must still clear the 0.7 default
	// threshold after normalization (SPEC track-matching REQ-TM-041).
	// This drives the real findBestFuzzyMatch (weights, bonus, candidate
	// selection), not just normalizeForMatch+similarity.
	variants := []struct {
		source  string
		library string
	}{
		{"Song Title (Remastered)", "Song Title"},
		{"Amazing Song (Radio Edit)", "Amazing Song"},
		{"Track Name - Remastered", "Track Name"},
		{"Ballad [Live]", "Ballad"},
		// Year-qualified forms from SPEC track-matching scenario 3.
		{"Don't Stop Me Now - Remastered 2011", "Don't Stop Me Now"},
		{"Bohemian Rhapsody - 2011 Remaster", "Bohemian Rhapsody"},
		{"Killer Queen (Remastered 2011)", "Killer Queen"},
	}
	const artist = "The Artist"
	for _, v := range variants {
		want := &ent.Track{Name: v.library}
		candidates := []NormalizedCandidate{
			{
				Track:       &ent.Track{Name: "Unrelated Song"},
				TitleRunes:  []rune(normalizeTitleForMatch("Unrelated Song")),
				ArtistRunes: []rune(normalizeForMatch("Someone Else")),
			},
			{
				Track:       want,
				TitleRunes:  []rune(normalizeTitleForMatch(v.library)),
				ArtistRunes: []rune(normalizeForMatch(artist)),
			},
		}
		best, score := findBestFuzzyMatch(
			[]rune(normalizeTitleForMatch(v.source)),
			[]rune(normalizeForMatch(artist)),
			candidates)
		if best != want {
			t.Fatalf("expected suffix variant %q to fuzzy-match %q, got %v (score %f)",
				v.source, v.library, best, score)
		}
		if score < 0.7 {
			t.Fatalf("expected suffix variant %q vs %q to clear the 0.7 threshold, got %f",
				v.source, v.library, score)
		}
	}
}

func TestFindBestFuzzyMatch_TitleWeightedOverArtist(t *testing.T) {
	// Title and artist share no runes, so transposing the source title/artist
	// arguments (or the candidate rune fields) changes the score and fails
	// this test. Pins the 0.6/0.4 weighting of REQ-TM-031.
	trk := &ent.Track{Name: "abcdef"}
	candidates := []NormalizedCandidate{
		{Track: trk, TitleRunes: []rune("abcdef"), ArtistRunes: []rune("qqqq")},
	}

	best, score := findBestFuzzyMatch([]rune("abcdef"), []rune("zzzz"), candidates)
	if best != trk {
		t.Fatalf("expected the only candidate to win, got %v", best)
	}
	// titleScore 1.0 * 0.6 + artistScore 0.0 * 0.4 = 0.6 exactly.
	if score < 0.599 || score > 0.601 {
		t.Fatalf("expected score 0.6 (title-weighted); got %f — title/artist arguments may be transposed", score)
	}
}

func TestFindBestFuzzyMatch_DualConfidenceBonus(t *testing.T) {
	// Pins the +0.1 bonus of REQ-TM-032: title and artist each one edit off
	// on 10-rune strings score 0.9 > 0.8, so the bonus applies.
	trk := &ent.Track{Name: "abcdefghij"}
	candidates := []NormalizedCandidate{
		{Track: trk, TitleRunes: []rune("abcdefghij"), ArtistRunes: []rune("klmnopqrst")},
	}

	_, score := findBestFuzzyMatch([]rune("abcdefghiX"), []rune("klmnopqrsX"), candidates)
	// 0.9*0.6 + 0.9*0.4 = 0.9, + 0.1 bonus = 1.0.
	if score < 0.999 {
		t.Fatalf("expected dual-confidence bonus to lift score to 1.0, got %f", score)
	}
}

func TestNormalizeForMatch_Regression_YearQualifiedRemaster(t *testing.T) {
	// SPEC track-matching scenario 3 requires normalization to strip
	// " - Remastered 2011"; strings.TrimSuffix alone cannot (issue found in
	// review of #357: the scenario previously passed only by a float hair).
	cases := []struct {
		input string
		want  string
	}{
		{"Don't Stop Me Now - Remastered 2011", "dont stop me now"},
		{"Bohemian Rhapsody - 2011 Remaster", "bohemian rhapsody"},
		{"Killer Queen (Remastered 2011)", "killer queen"},
		{"Somebody to Love [2009 Remaster]", "somebody to love"},
		{"Track Name - Remaster", "track name"},
		// Titles merely containing the word stay intact.
		{"Puppet Master", "puppet master"},
		{"Remaster", "remaster"},
	}
	for _, c := range cases {
		if got := normalizeForMatch(c.input); got != c.want {
			t.Fatalf("normalizeForMatch(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestNormalizeForMatch_Regression_NFCAndNFDEquivalent(t *testing.T) {
	// NFC and NFD encodings of the same text must normalize identically;
	// otherwise NFD-tagged libraries (macOS-ripped files) miss the exact tier
	// and, post rune-fix, score 0.0 in the fuzzy tier for Hangul.
	cases := []struct{ nfc, nfd string }{
		{"beyonc\u00e9", "beyonce\u0301"},                  // é vs e + combining acute
		{"\ud55c", "\u1112\u1161\u11ab"},                   // Hangul syllable vs conjoining jamo
		{"bj\u00f6rk j\u00f3ga", "bjo\u0308rk jo\u0301ga"}, // multiple combining marks
	}
	for _, c := range cases {
		if a, b := normalizeForMatch(c.nfc), normalizeForMatch(c.nfd); a != b {
			t.Fatalf("normalizeForMatch NFC %q = %q but NFD form = %q; want identical", c.nfc, a, b)
		}
	}
}

func TestNormalizeTitleForMatch_SymbolOnlyTitlesStayDistinct(t *testing.T) {
	// Symbol-only titles ("!!!", "???") must not all collapse to "" — that
	// made them exact-match each other at confidence 1.0 via the shared
	// "artist|" key and the both-empty similarity fast path.
	a := normalizeTitleForMatch("!!!")
	b := normalizeTitleForMatch("???")
	if a == "" || b == "" {
		t.Fatalf("symbol-only titles must not normalize to empty: %q -> %q, %q -> %q", "!!!", a, "???", b)
	}
	if a == b {
		t.Fatalf("distinct symbol-only titles must stay distinct, both normalized to %q", a)
	}
	if score := runeSim(a, b); score > 0.05 {
		t.Fatalf("expected ~0.0 similarity for distinct symbol-only titles, got %f", score)
	}
	// Identical symbol-only titles still match themselves.
	if score := runeSim(a, normalizeTitleForMatch("!!!")); score != 1.0 {
		t.Fatalf("identical symbol-only titles must score 1.0, got %f", score)
	}
	// Titles with letters are unaffected by the fallback.
	if got := normalizeTitleForMatch("Song Title (Remastered)"); got != "song title" {
		t.Fatalf("normalizeTitleForMatch letters path = %q, want %q", got, "song title")
	}
}
