package database

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/core"

	"gogs.io/gogs/internal/conf"
)

// setupLegacyTestEngine points the global XORM engine at a fresh SQLite
// database and syncs all legacy tables, including the soft-deletion archive
// tables.
func setupLegacyTestEngine(t *testing.T) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "soft-delete-test.db")
	prevDB := conf.Database
	prevSQLite := conf.UseSQLite3
	conf.UseSQLite3 = true
	conf.Database = conf.DatabaseOpts{
		Type: "sqlite3",
		Path: dbPath,
	}
	t.Cleanup(func() {
		conf.Database = prevDB
		conf.UseSQLite3 = prevSQLite
		if x != nil {
			_ = x.Close()
			x = nil
		}
	})

	var err error
	x, err = getEngine()
	require.NoError(t, err)
	x.SetMapper(core.GonicMapper{})
	// Access is normally created by GORM AutoMigrate rather than XORM sync.
	tables := make([]any, 0, len(legacyTables)+1)
	tables = append(tables, legacyTables...)
	tables = append(tables, new(Access))
	require.NoError(t, x.StoreEngine("InnoDB").Sync2(tables...))
}

func TestSoftDeleteAndRestoreRepository(t *testing.T) {
	setupLegacyTestEngine(t)
	conf.SetMockRepository(t, conf.RepositoryOpts{
		DeleteRestoreTime: time.Hour,
	})

	const (
		ownerID        = int64(1)
		collaboratorID = int64(2)
		repoID         = int64(1)
	)

	insertUser := func(id int64, name string) {
		t.Helper()
		email := name + "@example.com"
		_, err := x.Insert(&User{
			ID:          id,
			LowerName:   name,
			Name:        name,
			Email:       email,
			Password:    "password",
			Avatar:      email,
			AvatarEmail: email,
			IsActive:    true,
			NumRepos:    1,
		})
		require.NoError(t, err)
	}
	insertUser(ownerID, "alice")
	insertUser(collaboratorID, "bob")

	_, err := x.Insert(&Repository{
		ID:            repoID,
		OwnerID:       ownerID,
		Name:          "repo",
		LowerName:     "repo",
		DefaultBranch: "master",
	})
	require.NoError(t, err)

	_, err = x.Insert(&Collaboration{ID: 1, UserID: collaboratorID, RepoID: repoID, Mode: AccessModeWrite})
	require.NoError(t, err)
	_, err = x.Insert(&Access{UserID: collaboratorID, RepoID: repoID, Mode: AccessModeWrite})
	require.NoError(t, err)
	_, err = x.Insert(&Webhook{ID: 1, RepoID: repoID, URL: "http://example.com/hook", Events: `{"push_only":true}`, HookTaskType: GOGS, IsActive: true})
	require.NoError(t, err)
	_, err = x.Insert(&ProtectBranch{ID: 1, RepoID: repoID, Name: "master", Protected: true, RequirePullRequest: true})
	require.NoError(t, err)
	_, err = x.Insert(&ProtectBranchWhitelist{ID: 1, ProtectBranchID: 1, RepoID: repoID, Name: "master", UserID: collaboratorID})
	require.NoError(t, err)

	countRows := func(t *testing.T, bean any, cond string, args ...any) int64 {
		t.Helper()
		n, err := x.Where(cond, args...).Count(bean)
		require.NoError(t, err)
		return n
	}

	// Soft delete records the deleter, time and deadline, hides the repository
	// and archives its relations.
	require.NoError(t, SoftDeleteRepository(ownerID, ownerID, repoID))

	deleted, err := GetRepositoryByID(repoID)
	require.NoError(t, err)
	assert.True(t, deleted.IsDeleted())
	assert.Equal(t, ownerID, deleted.DeletedByID)
	assert.Greater(t, deleted.DeletedUnix, int64(0))
	assert.WithinDuration(t,
		time.Unix(deleted.DeletedUnix, 0).Add(time.Hour),
		time.Unix(deleted.RestoreDeadlineUnix, 0),
		time.Minute,
	)

	_, err = GetRepositoryByName(ownerID, "repo")
	assert.True(t, IsErrRepoNotExist(err))

	repos, err := GetUserRepositories(&UserRepoOptions{UserID: ownerID, Private: true})
	require.NoError(t, err)
	assert.Empty(t, repos)
	_, count, err := SearchRepositoryByName(&SearchRepoOptions{OwnerID: ownerID, Private: true, Page: 1, PageSize: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)

	assert.Equal(t, int64(0), countRows(t, new(Collaboration), "repo_id = ?", repoID))
	assert.Equal(t, int64(1), countRows(t, new(DeletedCollaboration), "repo_id = ?", repoID))
	assert.Equal(t, int64(0), countRows(t, new(Webhook), "repo_id = ?", repoID))
	assert.Equal(t, int64(1), countRows(t, new(DeletedWebhook), "repo_id = ?", repoID))
	assert.Equal(t, int64(0), countRows(t, new(ProtectBranch), "repo_id = ?", repoID))
	assert.Equal(t, int64(1), countRows(t, new(DeletedProtectBranch), "repo_id = ?", repoID))
	assert.Equal(t, int64(0), countRows(t, new(ProtectBranchWhitelist), "repo_id = ?", repoID))
	assert.Equal(t, int64(1), countRows(t, new(DeletedProtectBranchWhitelist), "repo_id = ?", repoID))
	assert.Equal(t, int64(0), countRows(t, new(Access), "repo_id = ?", repoID))

	ownerNumRepos := func(t *testing.T) int64 {
		t.Helper()
		owner, err := getUserByID(x, ownerID)
		require.NoError(t, err)
		return int64(owner.NumRepos)
	}
	assert.Equal(t, int64(0), ownerNumRepos(t))

	// Double soft deletion is rejected.
	assert.True(t, IsErrRepoAlreadyDeleted(SoftDeleteRepository(ownerID, ownerID, repoID)))

	// Restore brings every relation back atomically within the deadline.
	require.NoError(t, RestoreRepository(ownerID, ownerID, repoID))

	restored, err := GetRepositoryByName(ownerID, "repo")
	require.NoError(t, err)
	assert.False(t, restored.IsDeleted())
	assert.Equal(t, int64(0), restored.DeletedByID)
	assert.Equal(t, int64(0), restored.RestoreDeadlineUnix)

	collaboration := &Collaboration{ID: 1}
	has, err := x.Get(collaboration)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, AccessModeWrite, collaboration.Mode)
	assert.Equal(t, collaboratorID, collaboration.UserID)

	webhook := &Webhook{ID: 1}
	has, err = x.Get(webhook)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, "http://example.com/hook", webhook.URL)
	assert.Equal(t, `{"push_only":true}`, webhook.Events)
	assert.True(t, webhook.IsActive)
	assert.Equal(t, GOGS, webhook.HookTaskType)

	protectBranch := &ProtectBranch{ID: 1}
	has, err = x.Get(protectBranch)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, "master", protectBranch.Name)
	assert.True(t, protectBranch.Protected)
	assert.True(t, protectBranch.RequirePullRequest)

	whitelist := &ProtectBranchWhitelist{ID: 1}
	has, err = x.Get(whitelist)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, collaboratorID, whitelist.UserID)

	access := &Access{UserID: collaboratorID, RepoID: repoID}
	has, err = x.Get(access)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, AccessModeWrite, access.Mode)

	assert.Equal(t, int64(0), countRows(t, new(DeletedCollaboration), "repo_id = ?", repoID))
	assert.Equal(t, int64(0), countRows(t, new(DeletedWebhook), "repo_id = ?", repoID))
	assert.Equal(t, int64(0), countRows(t, new(DeletedProtectBranch), "repo_id = ?", repoID))
	assert.Equal(t, int64(0), countRows(t, new(DeletedProtectBranchWhitelist), "repo_id = ?", repoID))
	assert.Equal(t, int64(1), ownerNumRepos(t))

	// Restore is rejected once the deadline has passed.
	require.NoError(t, SoftDeleteRepository(ownerID, ownerID, repoID))
	_, err = x.Exec("UPDATE repository SET restore_deadline_unix = ? WHERE id = ?",
		time.Now().Add(-time.Minute).Unix(), repoID)
	require.NoError(t, err)
	err = RestoreRepository(ownerID, ownerID, repoID)
	assert.True(t, IsErrRepoRestoreExpired(err))
}
