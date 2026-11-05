package database

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/markup"
	"gogs.io/gogs/internal/osx"
)

func TestRepository_ComposeMetas(t *testing.T) {
	repo := &Repository{
		Name: "testrepo",
		Owner: &User{
			Name: "testuser",
		},
		ExternalTrackerFormat: "https://someurl.com/{user}/{repo}/{issue}",
	}

	t.Run("no external tracker is configured", func(t *testing.T) {
		repo.EnableExternalTracker = false

		metas := repo.ComposeMetas()
		assert.Equal(t, metas["repoLink"], repo.Link())

		// Should not have format and style if no external tracker is configured
		_, ok := metas["format"]
		assert.False(t, ok)
		_, ok = metas["style"]
		assert.False(t, ok)
	})

	t.Run("an external issue tracker is configured", func(t *testing.T) {
		repo.ExternalMetas = nil
		repo.EnableExternalTracker = true

		// Default to numeric issue style
		assert.Equal(t, markup.IssueNameStyleNumeric, repo.ComposeMetas()["style"])
		repo.ExternalMetas = nil

		repo.ExternalTrackerStyle = markup.IssueNameStyleNumeric
		assert.Equal(t, markup.IssueNameStyleNumeric, repo.ComposeMetas()["style"])
		repo.ExternalMetas = nil

		repo.ExternalTrackerStyle = markup.IssueNameStyleAlphanumeric
		assert.Equal(t, markup.IssueNameStyleAlphanumeric, repo.ComposeMetas()["style"])
		repo.ExternalMetas = nil

		metas := repo.ComposeMetas()
		assert.Equal(t, "testuser", metas["user"])
		assert.Equal(t, "testrepo", metas["repo"])
		assert.Equal(t, "https://someurl.com/{user}/{repo}/{issue}", metas["format"])
	})
}

func Test_CreateRepository_PreventDeletion(t *testing.T) {
	tempRepositoryRoot := filepath.Join(os.TempDir(), "createRepository-tempRepositoryRoot")
	conf.SetMockRepository(
		t,
		conf.RepositoryOpts{
			Root: tempRepositoryRoot,
		},
	)
	err := os.RemoveAll(tempRepositoryRoot)
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempRepositoryRoot) }()

	owner := &User{Name: "testuser"}
	opts := CreateRepoOptionsLegacy{Name: "safety-test"}
	repoPath := RepoPath(owner.Name, opts.Name)
	require.NoError(t, os.MkdirAll(repoPath, os.ModePerm))

	canary := filepath.Join(repoPath, "canary.txt")
	require.NoError(t, os.WriteFile(canary, []byte("should survive"), 0o644))

	_, err = CreateRepository(owner, owner, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository directory already exists")
	assert.True(t, osx.Exist(canary))
}

func TestIsRepoNameAllowed(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "repo", wantErr: false},
		{name: "my-repo.123", wantErr: false},
		{name: ".", wantErr: true},
		{name: "..", wantErr: true},
		{name: "repo.git", wantErr: true},
		{name: "repo.wiki", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.wantErr {
				assert.Error(t, IsRepoNameAllowed(test.name))
			} else {
				assert.NoError(t, IsRepoNameAllowed(test.name))
			}
		})
	}
}

func skipIfGitUnavailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found in PATH")
	}
}

func runGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestCheckMigrateRemoteAddress(t *testing.T) {
	skipIfGitUnavailable(t)

	t.Run("accessible local repository", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "src")
		require.NoError(t, os.MkdirAll(src, os.ModePerm))
		runGitCommand(t, src, "init")
		runGitCommand(t, src, "config", "user.email", "test@example.com")
		runGitCommand(t, src, "config", "user.name", "Test")
		require.NoError(t, os.WriteFile(filepath.Join(src, "README.md"), []byte("hello"), 0o644))
		runGitCommand(t, src, "add", "README.md")
		runGitCommand(t, src, "commit", "-m", "initial commit")

		assert.NoError(t, CheckMigrateRemoteAddress(30*time.Second, src))
	})

	t.Run("rejected credentials", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(server.Close)

		remoteAddr := strings.Replace(server.URL, "http://", "http://alice:wrongpass@", 1) + "/repo.git"
		err := CheckMigrateRemoteAddress(30*time.Second, remoteAddr)
		require.Error(t, err)
		assert.True(t, IsErrMigrateRemoteAuthFailed(err), err.Error())
	})

	t.Run("unreachable address without leaking credentials", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := listener.Addr().String()
		require.NoError(t, listener.Close())

		remoteAddr := "http://alice:supersecret@" + addr + "/repo.git"
		err = CheckMigrateRemoteAddress(10*time.Second, remoteAddr)
		require.Error(t, err)
		assert.False(t, IsErrMigrateRemoteAuthFailed(err))
		assert.NotContains(t, err.Error(), "supersecret")
	})
}
