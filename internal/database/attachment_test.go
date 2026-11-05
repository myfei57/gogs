package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/core"
	"xorm.io/xorm"
)

func TestDuplicateAttachments(t *testing.T) {
	tests := []struct {
		name        string
		attachments []*Attachment
		want        []DuplicateAttachment
	}{
		{
			name:        "no attachments",
			attachments: nil,
			want:        []DuplicateAttachment{},
		},
		{
			name: "single attachment",
			attachments: []*Attachment{
				{ID: 1, Name: "a.zip"},
			},
			want: []DuplicateAttachment{},
		},
		{
			name: "all names unique",
			attachments: []*Attachment{
				{ID: 1, Name: "a.zip"},
				{ID: 2, Name: "b.zip"},
			},
			want: []DuplicateAttachment{},
		},
		{
			name: "one duplicate group with unsorted IDs",
			attachments: []*Attachment{
				{ID: 9, Name: "a.zip"},
				{ID: 3, Name: "a.zip"},
				{ID: 7, Name: "b.zip"},
			},
			want: []DuplicateAttachment{
				{Name: "a.zip", AttachmentIDs: []int64{3, 9}},
			},
		},
		{
			name: "grouping is case sensitive",
			attachments: []*Attachment{
				{ID: 1, Name: "File.zip"},
				{ID: 2, Name: "File.zip"},
				{ID: 3, Name: "file.zip"},
				{ID: 4, Name: "file.zip"},
			},
			want: []DuplicateAttachment{
				{Name: "File.zip", AttachmentIDs: []int64{1, 2}},
				{Name: "file.zip", AttachmentIDs: []int64{3, 4}},
			},
		},
		{
			name: "empty names are grouped separately and sorted first",
			attachments: []*Attachment{
				{ID: 5, Name: "z.zip"},
				{ID: 6, Name: "z.zip"},
				{ID: 7, Name: ""},
				{ID: 8, Name: ""},
			},
			want: []DuplicateAttachment{
				{Name: "", AttachmentIDs: []int64{7, 8}},
				{Name: "z.zip", AttachmentIDs: []int64{5, 6}},
			},
		},
		{
			name: "three attachments share one name",
			attachments: []*Attachment{
				{ID: 30, Name: "a.zip"},
				{ID: 10, Name: "a.zip"},
				{ID: 20, Name: "a.zip"},
			},
			want: []DuplicateAttachment{
				{Name: "a.zip", AttachmentIDs: []int64{10, 20, 30}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := duplicateAttachments(test.attachments)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestGetDuplicateAttachmentsByReleaseID(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	engine, err := xorm.NewEngine("sqlite3", filepath.Join(t.TempDir(), "release_attachments.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	engine.SetMapper(core.GonicMapper{})
	require.NoError(t, engine.Sync2(new(Release), new(Attachment)))

	prevEngine := x
	x = engine
	t.Cleanup(func() { x = prevEngine })

	releases := []*Release{
		{ID: 1, RepoID: 1, PublisherID: 1, TagName: "v1.0", LowerTagName: "v1.0", Title: "v1.0"},
		{ID: 2, RepoID: 1, PublisherID: 1, TagName: "v2.0", LowerTagName: "v2.0", Title: "v2.0"},
		{ID: 3, RepoID: 1, PublisherID: 1, TagName: "v3.0", LowerTagName: "v3.0", Title: "v3.0", IsDraft: true},
	}
	for _, rel := range releases {
		_, err = x.Insert(rel)
		require.NoError(t, err)
	}

	attachments := []*Attachment{
		// Published release with duplicates, IDs are inserted out of order on
		// purpose to verify the result is sorted.
		{ID: 201, ReleaseID: 2, UUID: "uuid-201", Name: "a.zip"},
		{ID: 204, ReleaseID: 2, UUID: "uuid-204", Name: "a.zip"},
		{ID: 207, ReleaseID: 2, UUID: "uuid-207", Name: "A.zip"},
		{ID: 208, ReleaseID: 2, UUID: "uuid-208", Name: "A.zip"},
		{ID: 203, ReleaseID: 2, UUID: "uuid-203", Name: "b.zip"},
		{ID: 206, ReleaseID: 2, UUID: "uuid-206", Name: ""},
		{ID: 205, ReleaseID: 2, UUID: "uuid-205", Name: ""},

		// Draft release with duplicates.
		{ID: 301, ReleaseID: 3, UUID: "uuid-301", Name: "notes.pdf"},
		{ID: 303, ReleaseID: 3, UUID: "uuid-303", Name: "notes.pdf"},
		{ID: 302, ReleaseID: 3, UUID: "uuid-302", Name: "unique.bin"},
	}
	for _, attach := range attachments {
		_, err = x.Insert(attach)
		require.NoError(t, err)
	}

	t.Run("release without attachments", func(t *testing.T) {
		got, err := GetDuplicateAttachmentsByReleaseID(1)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("published release with duplicate names", func(t *testing.T) {
		got, err := GetDuplicateAttachmentsByReleaseID(2)
		require.NoError(t, err)
		assert.Equal(t, []DuplicateAttachment{
			{Name: "", AttachmentIDs: []int64{205, 206}},
			{Name: "A.zip", AttachmentIDs: []int64{207, 208}},
			{Name: "a.zip", AttachmentIDs: []int64{201, 204}},
		}, got)
	})

	t.Run("draft release follows the same rules", func(t *testing.T) {
		got, err := GetDuplicateAttachmentsByReleaseID(3)
		require.NoError(t, err)
		assert.Equal(t, []DuplicateAttachment{
			{Name: "notes.pdf", AttachmentIDs: []int64{301, 303}},
		}, got)
	})

	t.Run("unknown release", func(t *testing.T) {
		got, err := GetDuplicateAttachmentsByReleaseID(999)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestReleaseAttachmentReadAccess(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	db := newTestDB(t, "ReleaseAttachmentReadAccess")

	prevHandle := Handle
	Handle = &DB{db: db}
	t.Cleanup(func() { Handle = prevHandle })

	ctx := context.Background()
	const repoID, ownerID, readerID, outsiderID = 42, 99, 1, 2
	require.NoError(t, Handle.Permissions().SetRepoPerms(ctx, repoID, map[int64]AccessMode{
		readerID: AccessModeRead,
	}))

	privateRepo := &Repository{ID: repoID, OwnerID: ownerID, IsPrivate: true}
	assert.False(t, privateRepo.HasAccess(0), "anonymous user cannot access private repository")
	assert.False(t, privateRepo.HasAccess(outsiderID), "user without grant cannot access private repository")
	assert.True(t, privateRepo.HasAccess(readerID), "granted reader can access private repository")
	assert.True(t, privateRepo.HasAccess(ownerID), "owner can access private repository")

	publicRepo := &Repository{ID: 43, OwnerID: ownerID}
	assert.True(t, publicRepo.HasAccess(0), "anonymous user can read public repository")
	assert.True(t, publicRepo.HasAccess(outsiderID), "any user can read public repository")
}
