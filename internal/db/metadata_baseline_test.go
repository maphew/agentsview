package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A session that was renamed, starred, and pinned before the machine first
// opted into artifact sync must baseline that curation even while it sits in
// trash: only the soft delete would otherwise publish, and a later restore
// would reach peers without the name, star, or pin.
func TestMetadataBaselineSnapshotIncludesTrashedSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	require.NoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "proj", Machine: "local", Agent: "claude",
		MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, d.InsertMessages([]Message{{
		SessionID: "s1", Ordinal: 0, Role: "user", Content: "hi",
		ContentLength: 2, SourceUUID: "uuid-1",
	}}))
	name := "Kept name"
	require.NoError(t, d.RenameSession("s1", &name))
	starred, err := d.StarSession("s1")
	require.NoError(t, err)
	require.True(t, starred)
	msgs, err := d.GetAllMessages(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	note := "kept pin"
	_, err = d.PinMessage("s1", msgs[0].ID, &note)
	require.NoError(t, err)
	require.NoError(t, d.SoftDeleteSession("s1"))

	snap, err := d.MetadataBaselineSnapshot(ctx)
	require.NoError(t, err)

	require.Len(t, snap.Renames, 1)
	assert.Equal(t, "s1", snap.Renames[0].SessionID)
	require.NotNil(t, snap.Renames[0].DisplayName)
	assert.Equal(t, name, *snap.Renames[0].DisplayName)
	assert.Equal(t, []string{"s1"}, snap.StarredSessionIDs)
	assert.Equal(t, []string{"s1"}, snap.SoftDeletedIDs)
	require.Len(t, snap.Pins, 1)
	assert.Equal(t, "s1", snap.Pins[0].SessionID)
	assert.Equal(t, "uuid-1", snap.Pins[0].SourceUUID)
}
