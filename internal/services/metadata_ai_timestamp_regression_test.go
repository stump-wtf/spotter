package services

// Regression tests for the AI enrichment timestamp gate (issue #389, the
// spotter-6p7 class): a track must be handed to the LLM enricher at most once
// per enrichment window, and an AI pass that failed or returned nothing still
// has to record the attempt, otherwise the entity stays eligible forever and
// every 5-minute metadata sync spends money again.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"spotter/ent"
	"spotter/ent/enttest"
	"spotter/internal/config"
	"spotter/internal/enrichers"
	"spotter/internal/events"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingTrackEnricher stands in for the OpenAI enricher: it reports the
// configured result and counts how many times the pipeline consulted it.
type countingTrackEnricher struct {
	mu    sync.Mutex
	calls int
	data  *enrichers.TrackData
	err   error
}

func (f *countingTrackEnricher) Type() enrichers.Type { return enrichers.TypeOpenAI }
func (f *countingTrackEnricher) Name() string         { return "openai" }
func (f *countingTrackEnricher) IsAvailable() bool    { return true }

func (f *countingTrackEnricher) EnrichTrack(ctx context.Context, t *ent.Track) (*enrichers.TrackData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.data, f.err
}

func (f *countingTrackEnricher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// countingArtistEnricher is the artist-side counterpart, used to pin the
// summary-only case that spotter-6p7 fixed once before.
type countingArtistEnricher struct {
	mu    sync.Mutex
	calls int
	data  *enrichers.ArtistData
	err   error
}

func (f *countingArtistEnricher) Type() enrichers.Type { return enrichers.TypeOpenAI }
func (f *countingArtistEnricher) Name() string         { return "openai" }
func (f *countingArtistEnricher) IsAvailable() bool    { return true }
func (f *countingArtistEnricher) GetArtistImages(ctx context.Context, a *ent.Artist) ([]enrichers.ImageData, error) {
	return nil, nil
}

func (f *countingArtistEnricher) EnrichArtist(ctx context.Context, a *ent.Artist) (*enrichers.ArtistData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.data, f.err
}

func (f *countingArtistEnricher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newAITestService(client *ent.Client) *MetadataService {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewMetadataService(client, nil, &config.Config{}, logger, events.NewBus())
}

func registerAITestEnricher(svc *MetadataService, e enrichers.Enricher) {
	factory := func(ctx context.Context, u *ent.User) (enrichers.Enricher, error) {
		return e, nil
	}
	if err := svc.Register(enrichers.TypeOpenAI, factory); err != nil {
		panic(err)
	}
}

func newAITestUserAndTrack(t *testing.T, client *ent.Client, name string) (*ent.User, *ent.Track) {
	t.Helper()
	ctx := context.Background()
	u, err := client.User.Create().
		SetUsername("ai-gate-" + name).
		SetTheme("dark").
		Save(ctx)
	require.NoError(t, err)
	art, err := client.Artist.Create().
		SetName("AI Gate Artist " + name).
		SetUser(u).
		Save(ctx)
	require.NoError(t, err)
	tr, err := client.Track.Create().
		SetName("AI Gate Track " + name).
		SetArtist(art).
		Save(ctx)
	require.NoError(t, err)
	return u, tr
}

// TestTrackAIEnrichedAtRecordedOnZeroTags: Last.fm/model returned no new tags.
// The old code stamped last_ai_enriched_at only when AITags were non-empty, so
// the track stayed eligible and was re-enriched on the next sync.
func TestTrackAIEnrichedAtRecordedOnZeroTags(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:ai_ts_zero_tags?mode=memory&cache=shared&_fk=1")
	defer client.Close()

	ctx := context.Background()
	u, tr := newAITestUserAndTrack(t, client, "zero-tags")

	fake := &countingTrackEnricher{data: &enrichers.TrackData{AISummary: "A summary, no new tags"}}
	svc := newAITestService(client)
	registerAITestEnricher(svc, fake)

	_, err := svc.EnrichTracks(ctx, u)
	require.NoError(t, err)

	refreshed, err := client.Track.Get(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, fake.callCount(), "AI enricher should be consulted once")
	assert.Equal(t, "A summary, no new tags", refreshed.AiSummary)
	assert.NotNil(t, refreshed.LastAiEnrichedAt,
		"an AI pass with zero tags must still record the attempt (issue #389)")
}

// TestTrackAIEnrichedAtRecordedOnError: a failed AI call (blocked or rotated
// key, timeout) has to move the timestamp too, otherwise the track is retried
// on every sync forever, which is what turned a loop into a cost incident.
func TestTrackAIEnrichedAtRecordedOnError(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:ai_ts_error_path?mode=memory&cache=shared&_fk=1")
	defer client.Close()

	ctx := context.Background()
	u, tr := newAITestUserAndTrack(t, client, "error-path")

	fake := &countingTrackEnricher{err: errors.New("openai: api key blocked")}
	svc := newAITestService(client)
	registerAITestEnricher(svc, fake)

	_, err := svc.EnrichTracks(ctx, u)
	require.NoError(t, err)

	refreshed, err := client.Track.Get(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, fake.callCount())
	assert.NotNil(t, refreshed.LastAiEnrichedAt,
		"a failed AI pass must still record the attempt so the gate engages (issue #389)")
}

// TestTrackNotReEnrichedWithinAIWindow: the track query also selects rows for
// non-AI reasons (Lidarr pending, stale last_enriched_at), so the AI enricher
// must be gated per entity. This is the spend loop itself: two back-to-back
// passes may spend at most one completion per track.
func TestTrackNotReEnrichedWithinAIWindow(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:ai_ts_gate?mode=memory&cache=shared&_fk=1")
	defer client.Close()

	ctx := context.Background()
	u, tr := newAITestUserAndTrack(t, client, "gate")

	fake := &countingTrackEnricher{data: &enrichers.TrackData{AISummary: "first pass"}}
	svc := newAITestService(client)
	registerAITestEnricher(svc, fake)

	_, err := svc.EnrichTracks(ctx, u)
	require.NoError(t, err)
	require.Equal(t, 1, fake.callCount())

	// The row is still selected (Lidarr id is still nil), the AI enricher is not.
	_, err = svc.EnrichTracks(ctx, u)
	require.NoError(t, err)
	assert.Equal(t, 1, fake.callCount(), "track was re-enriched inside the AI window (issue #389)")

	refreshed, err := client.Track.Get(ctx, tr.ID)
	require.NoError(t, err)
	require.NotNil(t, refreshed.LastAiEnrichedAt)

	// Once the window has passed the track becomes eligible again.
	stale := time.Now().Add(-aiEnrichmentInterval - time.Hour)
	_, err = client.Track.UpdateOne(refreshed).SetLastAiEnrichedAt(stale).Save(ctx)
	require.NoError(t, err)

	_, err = svc.EnrichTracks(ctx, u)
	require.NoError(t, err)
	assert.Equal(t, 2, fake.callCount(), "track should be eligible again after the AI window")
}

// TestArtistAIEnrichedAtRecordedOnSummaryOnly: spotter-6p7 recorded the
// timestamp whenever any AI field was set; stamping only on AITags brought the
// loop back for artists whose enrichment produced a summary and no tags.
func TestArtistAIEnrichedAtRecordedOnSummaryOnly(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:ai_ts_artist?mode=memory&cache=shared&_fk=1")
	defer client.Close()

	ctx := context.Background()
	u, err := client.User.Create().
		SetUsername("ai-gate-artist").
		SetTheme("dark").
		Save(ctx)
	require.NoError(t, err)
	art, err := client.Artist.Create().
		SetName("Summary Only Artist").
		SetUser(u).
		Save(ctx)
	require.NoError(t, err)

	fake := &countingArtistEnricher{data: &enrichers.ArtistData{AISummary: "Summary, no tags"}}
	svc := newAITestService(client)
	registerAITestEnricher(svc, fake)

	_, err = svc.EnrichArtists(ctx, u)
	require.NoError(t, err)

	refreshed, err := client.Artist.Get(ctx, art.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, fake.callCount())
	assert.NotNil(t, refreshed.LastAiEnrichedAt,
		"a summary-only AI pass must still record the attempt (issue #389)")

	_, err = svc.EnrichArtists(ctx, u)
	require.NoError(t, err)
	assert.Equal(t, 1, fake.callCount(), "artist was re-enriched inside the AI window (issue #389)")
}
