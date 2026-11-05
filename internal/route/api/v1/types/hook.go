package types

import "time"

type RepositoryHook struct {
	ID      int64             `json:"id"`
	Type    string            `json:"type"`
	URL     string            `json:"-"`
	Config  map[string]string `json:"config"`
	Events  []string          `json:"events"`
	Active  bool              `json:"active"`
	Updated time.Time         `json:"updated_at"`
	Created time.Time         `json:"created_at"`
}

// RepositoryHookStatusCount counts webhooks in one enabled state grouped by
// their last delivery status.
type RepositoryHookStatusCount struct {
	Total          int `json:"total"`
	NeverDelivered int `json:"never_delivered"`
	Succeed        int `json:"succeed"`
	Failed         int `json:"failed"`
}

// RepositoryHookStatusSummary counts webhooks of a repository grouped by
// enabled state and last delivery status. It never contains webhook targets,
// secrets or delivery payloads.
type RepositoryHookStatusSummary struct {
	Total    int                       `json:"total"`
	Active   RepositoryHookStatusCount `json:"active"`
	Disabled RepositoryHookStatusCount `json:"disabled"`
}

type RepositoryDeployKey struct {
	ID       int64     `json:"id"`
	Key      string    `json:"key"`
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	Created  time.Time `json:"created_at"`
	ReadOnly bool      `json:"read_only"`
}
