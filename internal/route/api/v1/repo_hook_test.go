package v1

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/macaron.v1"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/context"
	"gogs.io/gogs/internal/database"
	"gogs.io/gogs/internal/route/api/v1/types"
)

func newLegacyTestEngine(t *testing.T) {
	t.Helper()

	prevDB := conf.Database
	prevUseSQLite3 := conf.UseSQLite3
	conf.Database = conf.DatabaseOpts{
		Type: "sqlite3",
		Path: filepath.Join(t.TempDir(), "test.db"),
	}
	conf.UseSQLite3 = true
	t.Cleanup(func() {
		_ = database.CloseTestEngine()
		conf.Database = prevDB
		conf.UseSQLite3 = prevUseSQLite3
	})
	require.NoError(t, database.NewTestEngine())
}

// newHookSummaryApp wires the same handlers as the production route: the
// reqRepoAdmin permission check followed by the status summary handler.
func newHookSummaryApp(accessMode database.AccessMode, repoID int64) *macaron.Macaron {
	m := macaron.New()
	m.Use(macaron.Renderer())
	m.Use(func(ctx *macaron.Context) {
		appCtx := &context.Context{
			Context: ctx,
			Repo: &context.Repository{
				AccessMode: accessMode,
				Repository: &database.Repository{
					ID: repoID,
				},
			},
		}
		ctx.Map(appCtx)
		ctx.Map(&context.APIContext{Context: appCtx})
	})
	m.Group("/:username/:reponame/hooks", func() {
		m.Get("/status-summary", hookStatusSummary)
		// The static "status-summary" segment must coexist with the ":id"
		// wildcard and never be captured by it.
		m.Combo("/:id").
			Patch(func() { panic("PATCH \":id\" must not match the status-summary route") }).
			Delete(func() {})
	}, reqRepoAdmin())
	return m
}

func serveHookSummary(app *macaron.Macaron) *http.Response {
	r, err := http.NewRequest(http.MethodGet, "/alice/example/hooks/status-summary", nil)
	if err != nil {
		panic(err)
	}
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, r)
	return rr.Result()
}

func TestHookStatusSummary(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	newLegacyTestEngine(t)

	const repoID = int64(1)
	const emptyRepoID = int64(2)

	mustCreateHook := func(isActive bool, lastStatus database.HookStatus) {
		err := database.CreateWebhook(&database.Webhook{
			RepoID:       repoID,
			URL:          "https://example.com/payload",
			Secret:       "super-secret",
			IsActive:     isActive,
			LastStatus:   lastStatus,
			HookTaskType: database.GOGS,
		})
		require.NoError(t, err)
	}

	mustCreateHook(true, database.HookStatusSucceed)
	mustCreateHook(true, database.HookStatusFailed)
	mustCreateHook(true, database.HookStatusNone)
	mustCreateHook(false, database.HookStatusNone)

	want := types.RepositoryHookStatusSummary{
		Total: 4,
		Active: types.RepositoryHookStatusCount{
			Total:          3,
			NeverDelivered: 1,
			Succeed:        1,
			Failed:         1,
		},
		Disabled: types.RepositoryHookStatusCount{
			Total:          1,
			NeverDelivered: 1,
		},
	}

	t.Run("write access is not enough", func(t *testing.T) {
		resp := serveHookSummary(newHookSummaryApp(database.AccessModeWrite, repoID))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("no access is forbidden", func(t *testing.T) {
		resp := serveHookSummary(newHookSummaryApp(database.AccessModeRead, repoID))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)

		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Empty(t, string(raw), "no summary data must be returned without admin access")
	})

	t.Run("admin access returns the summary", func(t *testing.T) {
		resp := serveHookSummary(newHookSummaryApp(database.AccessModeOwner, repoID))
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var got types.RepositoryHookStatusSummary
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
		assert.Equal(t, want, got)
	})

	t.Run("summary never exposes webhook secrets", func(t *testing.T) {
		resp := serveHookSummary(newHookSummaryApp(database.AccessModeOwner, repoID))
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var body map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		for _, key := range []string{"url", "secret", "config", "request", "response"} {
			assert.NotContains(t, body, key, "summary must not expose %q", key)
		}
	})

	t.Run("empty repository returns zero counts", func(t *testing.T) {
		resp := serveHookSummary(newHookSummaryApp(database.AccessModeOwner, emptyRepoID))
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var got types.RepositoryHookStatusSummary
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
		assert.Equal(t, types.RepositoryHookStatusSummary{}, got)
	})
}
