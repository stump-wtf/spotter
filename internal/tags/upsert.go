// Governing: SPEC-0014 REQ "Enricher Integration", SPEC-0014 REQ "Denormalized Entity Tags Table",
// SPEC-0014 REQ "Data Migration", SPEC-0014 REQ "Tag Normalization"
package tags

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"spotter/ent"
	"spotter/ent/tag"
	"spotter/ent/user"
)

// TypedTag represents a tag with a specific type classification.
// Governing: SPEC-0014 REQ "Enricher Integration"
type TypedTag struct {
	Name string
	Type string // "id3", "genre", "ai", "label", "source"
}

// UpsertTagsForEntity creates or retrieves Tag entities for the given typed tags,
// associates them with the specified entity via Ent edges, and maintains the
// denormalized entity_tags table. Idempotent: safe to call multiple times.
// Governing: SPEC-0014 REQ "Enricher Integration", SPEC-0014 REQ "Denormalized Entity Tags Table",
// SPEC-0014 REQ "Tag Normalization"
func UpsertTagsForEntity(ctx context.Context, client *ent.Client, db *sql.DB, userID int, entityType string, entityID int, typed []TypedTag) error {
	for _, tt := range typed {
		if tt.Name == "" {
			continue
		}

		// Governing: SPEC-0014 REQ "Tag Normalization" — trim and normalize
		displayName := strings.TrimSpace(tt.Name)
		if displayName == "" {
			continue
		}
		normalized := Normalize(displayName)
		if normalized == "" {
			continue
		}

		tagType := tag.TagType(tt.Type)

		// Governing: #349 — query-then-create race: use a retry loop for
		// concurrent upserts of the same (name, type, user) tuple.
		t, err := upsertTag(ctx, client, userID, displayName, normalized, tagType)
		if err != nil {
			return fmt.Errorf("upsert tag %q (type %s): %w", displayName, tt.Type, err)
		}

		// Add entity to the tag's edge (idempotent — Ent ignores duplicate edges)
		switch entityType {
		case "artist":
			err = t.Update().AddArtistIDs(entityID).Exec(ctx)
		case "album":
			err = t.Update().AddAlbumIDs(entityID).Exec(ctx)
		case "track":
			err = t.Update().AddTrackIDs(entityID).Exec(ctx)
		default:
			return fmt.Errorf("unknown entity type: %s", entityType)
		}
		if err != nil {
			return fmt.Errorf("add %s edge for tag %q: %w", entityType, normalized, err)
		}

		// Upsert into denormalized entity_tags table (ON CONFLICT DO NOTHING for idempotency)
		if err := upsertEntityTag(ctx, db, userID, t.ID, tt.Type, normalized, entityType, entityID); err != nil {
			return fmt.Errorf("upsert entity_tag for tag %q: %w", normalized, err)
		}
	}
	return nil
}

// upsertTag looks up an existing tag or creates one, with a retry on
// unique-constraint conflicts from concurrent goroutines.
// Governing: #349 — upsert race
func upsertTag(ctx context.Context, client *ent.Client, userID int, displayName, normalized string, tagType tag.TagType) (*ent.Tag, error) {
	// Look up existing tag by (normalized_name, tag_type, user_id)
	t, err := client.Tag.Query().
		Where(
			tag.NormalizedNameEQ(normalized),
			tag.TagTypeEQ(tagType),
			tag.HasUserWith(user.IDEQ(userID)),
		).
		Only(ctx)

	if err == nil {
		return t, nil
	}

	if !ent.IsNotFound(err) {
		return nil, fmt.Errorf("query tag %q: %w", normalized, err)
	}

	// Create new tag
	t, err = client.Tag.Create().
		SetName(displayName).
		SetNormalizedName(normalized).
		SetTagType(tagType).
		SetUserID(userID).
		Save(ctx)
	if err == nil {
		return t, nil
	}

	// Governing: #349 — concurrent goroutine may have created the same tag
	// between our query and create. If the unique constraint fired, re-query.
	if ent.IsConstraintError(err) {
		t, queryErr := client.Tag.Query().
			Where(
				tag.NormalizedNameEQ(normalized),
				tag.TagTypeEQ(tagType),
				tag.HasUserWith(user.IDEQ(userID)),
			).
			Only(ctx)
		if queryErr != nil {
			return nil, fmt.Errorf("create tag %q (type %s) failed with constraint, then re-query failed: %w", displayName, tagType, queryErr)
		}
		return t, nil
	}

	return nil, fmt.Errorf("create tag %q (type %s): %w", displayName, tagType, err)
}

// upsertEntityTag inserts a row into entity_tags, ignoring conflicts for idempotency.
func upsertEntityTag(ctx context.Context, db *sql.DB, userID, tagID int, tagType, tagName, entityType string, entityID int) error {
	// Use INSERT ... ON CONFLICT DO NOTHING for idempotency.
	// This works for both PostgreSQL and SQLite.
	_, err := db.ExecContext(ctx,
		`INSERT INTO entity_tags (user_id, tag_id, tag_type, tag_name, entity_type, entity_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (tag_id, entity_type, entity_id) DO NOTHING`,
		userID, tagID, tagType, tagName, entityType, entityID,
	)
	return err
}