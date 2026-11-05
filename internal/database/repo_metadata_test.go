package database

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/core"
	"xorm.io/xorm"

	"gogs.io/gogs/internal/conf"
)

func TestRepoMetadataEventBus(t *testing.T) {
	bus := newRepoMetadataEventBus()

	ch1, unsubscribe1 := bus.subscribe()
	ch2, unsubscribe2 := bus.subscribe()
	defer unsubscribe2()

	events := []RepoMetadataInvalidationEvent{
		{RepoID: 1, Version: 1, Fields: RepoMetadataFieldName},
		{RepoID: 1, Version: 2, Fields: RepoMetadataFieldVisibility},
	}
	bus.publish(events...)

	for i, want := range events {
		select {
		case got := <-ch1:
			assert.Equal(t, want, got, "subscriber 1 event %d", i)
		case <-time.After(time.Second):
			t.Fatalf("subscriber 1 timed out waiting for event %d", i)
		}
		select {
		case got := <-ch2:
			assert.Equal(t, want, got, "subscriber 2 event %d", i)
		case <-time.After(time.Second):
			t.Fatalf("subscriber 2 timed out waiting for event %d", i)
		}
	}

	// Unsubscribing stops delivery and closes the channel.
	unsubscribe1()
	bus.publish(RepoMetadataInvalidationEvent{RepoID: 1, Version: 3})

	select {
	case _, ok := <-ch1:
		assert.False(t, ok, "channel of unsubscribed consumer should be closed")
	case <-time.After(time.Second):
		t.Fatal("channel of unsubscribed consumer was not closed")
	}
	select {
	case got := <-ch2:
		assert.Equal(t, int64(3), got.Version)
	case <-time.After(time.Second):
		t.Fatal("remaining consumer should still receive events")
	}
}

func TestRepoMetadataEventDeduper(t *testing.T) {
	deduper := NewRepoMetadataEventDeduper()

	event := func(repoID, version int64) RepoMetadataInvalidationEvent {
		return RepoMetadataInvalidationEvent{RepoID: repoID, Version: version}
	}

	assert.True(t, deduper.Accept(event(1, 1)), "first version")
	assert.False(t, deduper.Accept(event(1, 1)), "duplicated delivery of the same version")
	assert.False(t, deduper.Accept(event(1, 0)), "older version arriving out of order")
	assert.True(t, deduper.Accept(event(1, 2)), "newer version")

	// Versions are tracked independently per repository.
	assert.True(t, deduper.Accept(event(2, 1)), "other repository starts at its own version")
	assert.False(t, deduper.Accept(event(2, 1)), "duplicated delivery for the other repository")
}

func TestRepoMetadataField(t *testing.T) {
	assert.Equal(t, "none", RepoMetadataField(0).String())
	assert.Equal(t, "name|visibility", (RepoMetadataFieldName | RepoMetadataFieldVisibility).String())

	ev := RepoMetadataInvalidationEvent{Fields: RepoMetadataFieldDefaultBranch | RepoMetadataFieldArchiveStatus}
	assert.True(t, ev.Has(RepoMetadataFieldDefaultBranch))
	assert.True(t, ev.Has(RepoMetadataFieldArchiveStatus))
	assert.False(t, ev.Has(RepoMetadataFieldName))
}

func newMetadataTestEngine(t *testing.T) *xorm.Engine {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	engine, err := xorm.NewEngine("sqlite3", "file:"+dbPath+"?cache=shared&mode=rwc")
	require.NoError(t, err)
	engine.SetMapper(core.GonicMapper{})
	require.NoError(t, engine.Sync2(new(Repository), new(User), new(Action)))
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

func loadMetadataRepo(t *testing.T, engine Engine, repoID int64) *Repository {
	t.Helper()
	repo := new(Repository)
	has, err := engine.ID(repoID).Get(repo)
	require.NoError(t, err)
	require.True(t, has, "repository %d should exist", repoID)
	return repo
}

// TestBumpRepoMetadataVersion verifies that each tracked field advances the
// metadata version by one and is reported in the event mask, while unrelated
// fields do not bump the version.
func TestBumpRepoMetadataVersion(t *testing.T) {
	engine := newMetadataTestEngine(t)

	_, err := engine.InsertOne(&Repository{
		ID:            1,
		OwnerID:       1,
		Name:          "old",
		LowerName:     "old",
		DefaultBranch: "main",
	})
	require.NoError(t, err)

	cases := []struct {
		name    string
		apply   func(*Repository)
		fields  RepoMetadataField
		version int64
	}{
		{"name", func(r *Repository) { r.Name = "new"; r.LowerName = "new" }, RepoMetadataFieldName, 1},
		{"visibility", func(r *Repository) { r.IsPrivate = true }, RepoMetadataFieldVisibility, 2},
		{"default branch", func(r *Repository) { r.DefaultBranch = "develop" }, RepoMetadataFieldDefaultBranch, 3},
		{"archive status", func(r *Repository) { r.IsArchived = true }, RepoMetadataFieldArchiveStatus, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := loadMetadataRepo(t, engine, 1)
			tc.apply(repo)
			events, err := updateRepository(engine, repo, false)
			require.NoError(t, err)
			require.Len(t, events, 1)
			assert.Equal(t, int64(1), events[0].RepoID)
			assert.Equal(t, tc.version, events[0].Version)
			assert.Equal(t, tc.fields, events[0].Fields)
			assert.Equal(t, tc.version, loadMetadataRepo(t, engine, 1).MetadataVersion)
		})
	}

	t.Run("untracked field does not bump version", func(t *testing.T) {
		repo := loadMetadataRepo(t, engine, 1)
		repo.Description = "new description"
		events, err := updateRepository(engine, repo, false)
		require.NoError(t, err)
		assert.Empty(t, events)
		assert.Equal(t, int64(4), loadMetadataRepo(t, engine, 1).MetadataVersion)
	})
}

// metadataTestRepoID is the repository used by the commit/rollback test. It
// is deliberately large to avoid colliding with events emitted by other tests
// on the shared bus.
const metadataTestRepoID = int64(990001)

// waitForRepoEvent waits for an event targeting metadataTestRepoID and ignores
// events that other tests may emit on the shared bus.
func waitForRepoEvent(t *testing.T, ch <-chan RepoMetadataInvalidationEvent) RepoMetadataInvalidationEvent {
	t.Helper()
	for {
		select {
		case event := <-ch:
			if event.RepoID == metadataTestRepoID {
				return event
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for metadata event of repository %d", metadataTestRepoID)
		}
	}
}

func assertNoRepoEvent(t *testing.T, ch <-chan RepoMetadataInvalidationEvent) {
	t.Helper()
	for {
		select {
		case event := <-ch:
			if event.RepoID == metadataTestRepoID {
				t.Fatalf("unexpected metadata event for repository %d: %+v", metadataTestRepoID, event)
			}
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

// TestUpdateRepository_MetadataEvents verifies that invalidation events are
// published with an incrementing version only after the transaction commits,
// and that rolled back changes are neither persisted nor published.
func TestUpdateRepository_MetadataEvents(t *testing.T) {
	engine := newMetadataTestEngine(t)

	origEngine := x
	x = engine
	t.Cleanup(func() { x = origEngine })

	_, err := engine.InsertOne(&Repository{
		ID:            metadataTestRepoID,
		OwnerID:       1,
		Name:          "alpha",
		LowerName:     "alpha",
		DefaultBranch: "main",
		IsPrivate:     true,
	})
	require.NoError(t, err)
	_, err = engine.InsertOne(&User{
		ID:        1,
		Name:      "owner",
		LowerName: "owner",
		Email:     "owner@example.com",
	})
	require.NoError(t, err)

	conf.SetMockRepository(t, conf.RepositoryOpts{
		Root: t.TempDir(),
	})

	ch, unsubscribe := SubscribeRepoMetadataInvalidation()
	defer unsubscribe()

	t.Run("rollback does not publish or persist", func(t *testing.T) {
		sess := engine.NewSession()
		defer sess.Close()
		require.NoError(t, sess.Begin())

		repo := loadMetadataRepo(t, sess, metadataTestRepoID)
		repo.IsArchived = true
		events, err := updateRepository(sess, repo, false)
		require.NoError(t, err)
		require.Len(t, events, 1)
		assert.Equal(t, RepoMetadataFieldArchiveStatus, events[0].Fields)
		assert.Equal(t, int64(1), events[0].Version)

		require.NoError(t, sess.Rollback())

		assertNoRepoEvent(t, ch)
		persisted := loadMetadataRepo(t, engine, metadataTestRepoID)
		assert.False(t, persisted.IsArchived)
		assert.Equal(t, int64(0), persisted.MetadataVersion)
	})

	t.Run("committed name change publishes versioned event", func(t *testing.T) {
		repo := loadMetadataRepo(t, engine, metadataTestRepoID)
		repo.Name = "beta"
		repo.LowerName = "beta"
		require.NoError(t, UpdateRepository(repo, false))

		event := waitForRepoEvent(t, ch)
		assert.Equal(t, int64(1), event.Version)
		assert.Equal(t, RepoMetadataFieldName, event.Fields)

		persisted := loadMetadataRepo(t, engine, metadataTestRepoID)
		assert.Equal(t, "beta", persisted.Name)
		assert.Equal(t, int64(1), persisted.MetadataVersion)
	})

	t.Run("untracked change does not publish", func(t *testing.T) {
		repo := loadMetadataRepo(t, engine, metadataTestRepoID)
		repo.Description = "new description"
		require.NoError(t, UpdateRepository(repo, false))
		assertNoRepoEvent(t, ch)
		assert.Equal(t, int64(1), loadMetadataRepo(t, engine, metadataTestRepoID).MetadataVersion)
	})

	t.Run("multiple fields in one commit share one version", func(t *testing.T) {
		repo := loadMetadataRepo(t, engine, metadataTestRepoID)
		repo.DefaultBranch = "develop"
		repo.IsArchived = true
		require.NoError(t, UpdateRepository(repo, false))

		event := waitForRepoEvent(t, ch)
		assert.Equal(t, int64(2), event.Version)
		assert.True(t, event.Has(RepoMetadataFieldDefaultBranch))
		assert.True(t, event.Has(RepoMetadataFieldArchiveStatus))
	})

	t.Run("visibility change publishes after commit", func(t *testing.T) {
		repo := loadMetadataRepo(t, engine, metadataTestRepoID)
		repo.IsPrivate = false
		// The visibility side effects touch the repository directory.
		require.NoError(t, os.MkdirAll(repo.RepoPath(), os.ModePerm))
		require.NoError(t, UpdateRepository(repo, true))

		event := waitForRepoEvent(t, ch)
		assert.Equal(t, int64(3), event.Version)
		assert.True(t, event.Has(RepoMetadataFieldVisibility))
	})

	t.Run("consumer deduplicates redelivered events", func(t *testing.T) {
		committed := RepoMetadataInvalidationEvent{
			RepoID:  metadataTestRepoID,
			Version: 3,
			Fields:  RepoMetadataFieldVisibility,
		}
		deduper := NewRepoMetadataEventDeduper()
		require.True(t, deduper.Accept(committed))

		// Simulate at-least-once redelivery of the same committed event.
		repoMetadataEvents.publish(committed)
		redelivered := waitForRepoEvent(t, ch)
		assert.False(t, deduper.Accept(redelivered), "redelivered version must be deduplicated")
	})
}
