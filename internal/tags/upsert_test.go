package tags

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"spotter/ent"
	"spotter/ent/enttest"
	"spotter/ent/tag"
	"spotter/ent/user"
	"spotter/internal/database"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupUpsertTestDB(t *testing.T) (*ent.Client, *sql.DB) {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared&_fk=1"
	client := enttest.Open(t, "sqlite3", dsn)

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if err := database.CreateEntityTagsTable(context.Background(), "sqlite3", db); err != nil {
		t.Fatal(err)
	}

	return client, db
}

func createUpsertTestUser(t *testing.T, client *ent.Client) *ent.User {
	t.Helper()
	u := client.User.Create().SetUsername(fmt.Sprintf("upserttest_%s", t.Name())).SetPaginationSize(25).SaveX(context.Background())
	return u
}

// TestUpsertTagsForEntity_NameTrim ensures that tag names with leading/trailing
// whitespace are trimmed before being stored as the display name ("name" column).
// Regression for #349: "  shoegaze  " should be stored as "shoegaze", not verbatim.
func TestUpsertTagsForEntity_NameTrim(t *testing.T) {
	client, db := setupUpsertTestDB(t)
	u := createUpsertTestUser(t, client)
	ctx := context.Background()

	art, err := client.Artist.Create().
		SetName("Test Artist").
		SetUser(u).
		Save(ctx)
	require.NoError(t, err)

	typed := []TypedTag{
		{Name: "  shoegaze  ", Type: "ai"},
	}

	err = UpsertTagsForEntity(ctx, client, db, u.ID, "artist", art.ID, typed)
	require.NoError(t, err)

	// Verify the stored tag name is trimmed
	tags, err := client.Tag.Query().
		Where(tag.HasUserWith(user.IDEQ(u.ID))).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, tags, 1)

	// The display name MUST be trimmed
	assert.Equal(t, "shoegaze", tags[0].Name,
		"display name should be trimmed, not stored verbatim")
	// normalized name should already be trimmed+lowercased
	assert.Equal(t, "shoegaze", tags[0].NormalizedName)
}

// TestUpsertTagsForEntity_ConcurrentUpsertRace ensures that concurrent upserts
// of the same (name, type, user) tag both succeed instead of one failing with
// a unique constraint violation.
// Regression for #349: query-then-create race against the Tag unique index.
func TestUpsertTagsForEntity_ConcurrentUpsertRace(t *testing.T) {
	client, db := setupUpsertTestDB(t)
	u := createUpsertTestUser(t, client)
	ctx := context.Background()

	art, err := client.Artist.Create().
		SetName("Race Artist").
		SetUser(u).
		Save(ctx)
	require.NoError(t, err)

	typed := []TypedTag{
		{Name: "shoegaze", Type: "ai"},
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- UpsertTagsForEntity(ctx, client, db, u.ID, "artist", art.ID, typed)
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err, "concurrent upsert must not fail with unique constraint violation")
	}

	// Both should have succeeded; we should have exactly one tag
	tags, err := client.Tag.Query().
		Where(tag.HasUserWith(user.IDEQ(u.ID))).
		All(ctx)
	require.NoError(t, err)
	assert.Len(t, tags, 1, "concurrent upserts must produce exactly one tag, not duplicates")
}

// TestUpsertTagsForEntity_RegenerateAIBypass verifies that the shared helper
// correctly persists TypedTags when called from a regenerate handler.
// Regression for #349: regenerate handlers discarded data.TypedTags.
func TestUpsertTagsForEntity_RegenerateAIBypass(t *testing.T) {
	client, db := setupUpsertTestDB(t)
	u := createUpsertTestUser(t, client)
	ctx := context.Background()

	art, err := client.Artist.Create().
		SetName("Regen Artist").
		SetUser(u).
		Save(ctx)
	require.NoError(t, err)

	// Simulate what an AI enricher returns
	typed := []TypedTag{
		{Name: "dream pop", Type: "ai"},
		{Name: "ethereal", Type: "ai"},
		{Name: "indie", Type: "id3"},
	}

	err = UpsertTagsForEntity(ctx, client, db, u.ID, "artist", art.ID, typed)
	require.NoError(t, err)

	// Verify all tags were persisted
	tags, err := client.Tag.Query().
		Where(tag.HasUserWith(user.IDEQ(u.ID))).
		All(ctx)
	require.NoError(t, err)
	assert.Len(t, tags, 3, "all three typed tags should be persisted")

	// Verify entity_tags denormalized table has the rows
	rows, err := db.QueryContext(ctx,
		`SELECT tag_type, tag_name FROM entity_tags WHERE entity_type = ? AND entity_id = ?`,
		"artist", art.ID)
	require.NoError(t, err)
	defer rows.Close()

	var count int
	for rows.Next() {
		var tagType, tagName string
		require.NoError(t, rows.Scan(&tagType, &tagName))
		count++
	}
	assert.Equal(t, 3, count, "entity_tags should have all three rows")

	// Idempotency: calling again should not fail
	err = UpsertTagsForEntity(ctx, client, db, u.ID, "artist", art.ID, typed)
	require.NoError(t, err, "idempotent upsert should not fail on second call")

	tagsAfter, err := client.Tag.Query().
		Where(tag.HasUserWith(user.IDEQ(u.ID))).
		All(ctx)
	require.NoError(t, err)
	assert.Len(t, tagsAfter, 3, "idempotent upsert must not create duplicates")
}

// TestUpsertTagsForEntity_DifferentEntityTypes ensures the helper works for
// all supported entity types (artist, album, track).
func TestUpsertTagsForEntity_DifferentEntityTypes(t *testing.T) {
	client, db := setupUpsertTestDB(t)
	u := createUpsertTestUser(t, client)
	ctx := context.Background()

	// Create one of each entity type
	art, err := client.Artist.Create().SetName("Multi Artist").SetUser(u).Save(ctx)
	require.NoError(t, err)

	alb, err := client.Album.Create().SetName("Multi Album").SetUser(u).SetArtist(art).Save(ctx)
	require.NoError(t, err)

	trk, err := client.Track.Create().SetName("Multi Track").SetAlbum(alb).SetArtist(art).Save(ctx)
	require.NoError(t, err)

	typed := []TypedTag{{Name: "ambient", Type: "ai"}}

	// Artist
	require.NoError(t, UpsertTagsForEntity(ctx, client, db, u.ID, "artist", art.ID, typed))
	// Album
	require.NoError(t, UpsertTagsForEntity(ctx, client, db, u.ID, "album", alb.ID, typed))
	// Track
	require.NoError(t, UpsertTagsForEntity(ctx, client, db, u.ID, "track", trk.ID, typed))

	// Verify entity_tags has rows for all three
	for _, et := range []struct {
		etype string
		eid   int
	}{
		{"artist", art.ID}, {"album", alb.ID}, {"track", trk.ID},
	} {
		rows, err := db.QueryContext(ctx,
			`SELECT COUNT(*) FROM entity_tags WHERE entity_type = ? AND entity_id = ?`,
			et.etype, et.eid)
		require.NoError(t, err)
		var count int
		require.True(t, rows.Next())
		require.NoError(t, rows.Scan(&count))
		rows.Close()
		assert.Equal(t, 1, count, "entity_tags should have 1 row for %s %d", et.etype, et.eid)
	}
}

// TestUpsertTagsForEntity_EmptyNameSkipped ensures empty tag names are skipped.
func TestUpsertTagsForEntity_EmptyNameSkipped(t *testing.T) {
	client, db := setupUpsertTestDB(t)
	u := createUpsertTestUser(t, client)
	ctx := context.Background()

	art, err := client.Artist.Create().SetName("Empty Artist").SetUser(u).Save(ctx)
	require.NoError(t, err)

	typed := []TypedTag{
		{Name: "", Type: "ai"},
		{Name: "valid", Type: "ai"},
	}

	err = UpsertTagsForEntity(ctx, client, db, u.ID, "artist", art.ID, typed)
	require.NoError(t, err)

	tags, err := client.Tag.Query().
		Where(tag.HasUserWith(user.IDEQ(u.ID))).
		All(ctx)
	require.NoError(t, err)
	assert.Len(t, tags, 1, "empty name should be skipped, only valid tag persisted")
}