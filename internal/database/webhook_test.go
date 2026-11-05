package database

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xorm.io/core"
	"xorm.io/xorm"
)

func newWebhookTestEngine(t *testing.T) {
	t.Helper()

	dsn := "file:" + filepath.Join(t.TempDir(), "webhook_test.db") + "?cache=shared&mode=rwc"
	engine, err := xorm.NewEngine("sqlite3", dsn)
	require.NoError(t, err)
	engine.SetMapper(core.GonicMapper{})
	require.NoError(t, engine.Sync2(new(Webhook), new(HookTask)))

	prev := x
	x = engine
	t.Cleanup(func() {
		_ = engine.Close()
		x = prev
	})
}

func mustCreateStatusWebhook(t *testing.T, repoID int64, isActive bool, lastStatus HookStatus, secret string) {
	t.Helper()
	err := CreateWebhook(&Webhook{
		RepoID:       repoID,
		URL:          "https://example.com/hooks/" + secret,
		Secret:       secret,
		IsActive:     isActive,
		LastStatus:   lastStatus,
		HookTaskType: GOGS,
	})
	require.NoError(t, err)
}

func TestGetWebhookStatusSummaryByRepoID(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	newWebhookTestEngine(t)

	// A webhook of another repository is used to assert the summary is scoped
	// to the requested repository.
	mustCreateStatusWebhook(t, 999, true, HookStatusSucceed, "other-repo-secret")

	tests := []struct {
		name     string
		repoID   int64
		fixtures func(t *testing.T, repoID int64)
		want     WebhookStatusSummary
	}{
		{
			name:   "disabled",
			repoID: 10,
			fixtures: func(t *testing.T, repoID int64) {
				mustCreateStatusWebhook(t, repoID, false, HookStatusFailed, "disabled-secret")
			},
			want: WebhookStatusSummary{
				Total:    1,
				Disabled: WebhookStatusCount{Total: 1, Failed: 1},
			},
		},
		{
			name:   "last delivery succeeded",
			repoID: 11,
			fixtures: func(t *testing.T, repoID int64) {
				mustCreateStatusWebhook(t, repoID, true, HookStatusSucceed, "succeed-secret")
			},
			want: WebhookStatusSummary{
				Total:  1,
				Active: WebhookStatusCount{Total: 1, Succeed: 1},
			},
		},
		{
			name:   "last delivery failed",
			repoID: 12,
			fixtures: func(t *testing.T, repoID int64) {
				mustCreateStatusWebhook(t, repoID, true, HookStatusFailed, "failed-secret")
			},
			want: WebhookStatusSummary{
				Total:  1,
				Active: WebhookStatusCount{Total: 1, Failed: 1},
			},
		},
		{
			name:   "no delivery history",
			repoID: 13,
			fixtures: func(t *testing.T, repoID int64) {
				mustCreateStatusWebhook(t, repoID, true, HookStatusNone, "never-delivered-secret")
			},
			want: WebhookStatusSummary{
				Total:  1,
				Active: WebhookStatusCount{Total: 1, NeverDelivered: 1},
			},
		},
		{
			name:     "repository without webhooks",
			repoID:   14,
			fixtures: func(t *testing.T, repoID int64) {},
			want:     WebhookStatusSummary{},
		},
		{
			name:   "all enabled state and last status combinations",
			repoID: 20,
			fixtures: func(t *testing.T, repoID int64) {
				mustCreateStatusWebhook(t, repoID, true, HookStatusNone, "a1")
				mustCreateStatusWebhook(t, repoID, true, HookStatusNone, "a2")
				mustCreateStatusWebhook(t, repoID, true, HookStatusSucceed, "a3")
				mustCreateStatusWebhook(t, repoID, true, HookStatusFailed, "a4")
				mustCreateStatusWebhook(t, repoID, false, HookStatusNone, "d1")
				mustCreateStatusWebhook(t, repoID, false, HookStatusSucceed, "d2")
				mustCreateStatusWebhook(t, repoID, false, HookStatusFailed, "d3")
			},
			want: WebhookStatusSummary{
				Total: 7,
				Active: WebhookStatusCount{
					Total:          4,
					NeverDelivered: 2,
					Succeed:        1,
					Failed:         1,
				},
				Disabled: WebhookStatusCount{
					Total:          3,
					NeverDelivered: 1,
					Succeed:        1,
					Failed:         1,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.fixtures(t, test.repoID)

			got, err := GetWebhookStatusSummaryByRepoID(test.repoID)
			require.NoError(t, err)
			assert.Equal(t, &test.want, got)

			assert.Equal(t, got.Total, got.Active.Total+got.Disabled.Total)
			assert.Equal(t, got.Active.Total, got.Active.NeverDelivered+got.Active.Succeed+got.Active.Failed)
			assert.Equal(t, got.Disabled.Total, got.Disabled.NeverDelivered+got.Disabled.Succeed+got.Disabled.Failed)
		})
	}

	t.Run("does not read hook tasks or expose sensitive fields", func(t *testing.T) {
		// The summary must be produced without any delivery being triggered or
		// recorded.
		numTasks, err := x.Count(new(HookTask))
		require.NoError(t, err)
		assert.Zero(t, numTasks)

		summary, err := GetWebhookStatusSummaryByRepoID(10)
		require.NoError(t, err)

		raw, err := json.Marshal(summary)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		for _, key := range []string{"url", "secret", "config", "request", "response"} {
			assert.NotContains(t, body, key, "summary must not expose %q", key)
		}
	})
}
