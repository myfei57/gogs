package v1

import (
	stdctx "context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gogs.io/gogs/internal/database"
	"gogs.io/gogs/internal/lfsx"
)

func TestLFSBatchEndpoint(t *testing.T) {
	tests := []struct {
		name         string
		remoteAddr   string
		wantEndpoint string
		wantUsername string
		wantPassword string
		wantErr      bool
	}{
		{
			name:         "append git suffix",
			remoteAddr:   "https://example.com/owner/repo",
			wantEndpoint: "https://example.com/owner/repo.git/info/lfs/objects/batch",
		},
		{
			name:         "keep existing git suffix",
			remoteAddr:   "https://example.com/owner/repo.git",
			wantEndpoint: "https://example.com/owner/repo.git/info/lfs/objects/batch",
		},
		{
			name:         "strip trailing slash",
			remoteAddr:   "https://example.com/owner/repo/",
			wantEndpoint: "https://example.com/owner/repo.git/info/lfs/objects/batch",
		},
		{
			name:         "extract credentials and redact endpoint",
			remoteAddr:   "https://alice:secret@example.com/owner/repo.git",
			wantEndpoint: "https://example.com/owner/repo.git/info/lfs/objects/batch",
			wantUsername: "alice",
			wantPassword: "secret",
		},
		{
			name:         "strip query and fragment",
			remoteAddr:   "https://example.com/owner/repo?token=abc#LFS",
			wantEndpoint: "https://example.com/owner/repo.git/info/lfs/objects/batch",
		},
		{
			name:       "non http scheme",
			remoteAddr: "git://example.com/owner/repo.git",
			wantErr:    true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint, username, password, err := lfsBatchEndpoint(test.remoteAddr)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantEndpoint, endpoint)
			assert.Equal(t, test.wantUsername, username)
			assert.Equal(t, test.wantPassword, password)
			assert.NotContains(t, endpoint, "secret")
		})
	}
}

func TestProbeRemoteLFS(t *testing.T) {
	t.Run("supported with basic auth", func(t *testing.T) {
		var gotPath, gotAuth string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Contains(t, r.Header.Get("Accept"), precheckLFSContentType)
			assert.Equal(t, precheckLFSContentType, r.Header.Get("Content-Type"))
			w.Header().Set("Content-Type", precheckLFSContentType)
			_, _ = w.Write([]byte(`{"objects":[]}`))
		}))
		t.Cleanup(server.Close)

		status, err := probeRemoteLFS(stdctx.Background(), server.URL+"/owner/repo?foo=bar")
		require.NoError(t, err)
		assert.Equal(t, remoteLFSSupported, status)
		assert.Equal(t, "/owner/repo.git/info/lfs/objects/batch", gotPath)
		assert.Empty(t, gotAuth)
	})

	t.Run("credentials forwarded as basic auth", func(t *testing.T) {
		var gotAuth string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusBadRequest)
		}))
		t.Cleanup(server.Close)

		status, err := probeRemoteLFS(stdctx.Background(), strings.Replace(server.URL, "http://", "http://alice:secret@", 1)+"/owner/repo")
		require.NoError(t, err)
		assert.Equal(t, remoteLFSSupported, status)
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
		assert.Equal(t, wantAuth, gotAuth)
	})

	t.Run("unauthorized still means supported", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Lfs-Authenticate", `Basic realm="Git LFS"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(server.Close)

		status, err := probeRemoteLFS(stdctx.Background(), server.URL+"/owner/repo")
		require.NoError(t, err)
		assert.Equal(t, remoteLFSSupported, status)
	})

	t.Run("not found means unsupported", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(server.Close)

		status, err := probeRemoteLFS(stdctx.Background(), server.URL+"/owner/repo")
		require.NoError(t, err)
		assert.Equal(t, remoteLFSUnsupported, status)
	})

	t.Run("gone means unsupported", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGone)
		}))
		t.Cleanup(server.Close)

		status, err := probeRemoteLFS(stdctx.Background(), server.URL+"/owner/repo")
		require.NoError(t, err)
		assert.Equal(t, remoteLFSUnsupported, status)
	})

	t.Run("connection failure is undetermined without leaking credentials", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := listener.Addr().String()
		require.NoError(t, listener.Close())

		status, err := probeRemoteLFS(stdctx.Background(), "http://alice:supersecret@"+addr+"/owner/repo")
		require.Error(t, err)
		assert.Equal(t, remoteLFSUnknown, status)
		assert.NotContains(t, err.Error(), "supersecret")
	})

	t.Run("non http address", func(t *testing.T) {
		status, err := probeRemoteLFS(stdctx.Background(), "git://example.com/owner/repo.git")
		require.Error(t, err)
		assert.Equal(t, remoteLFSUnknown, status)
	})
}

func TestCheckLocalLFSStorage(t *testing.T) {
	t.Run("ready local storage", func(t *testing.T) {
		objectsPath := filepath.Join(t.TempDir(), "lfs-objects")
		err := checkLocalLFSStorage(lfsx.StorageLocal, objectsPath)
		require.NoError(t, err)
		assert.DirExists(t, objectsPath)

		entries, err := os.ReadDir(objectsPath)
		require.NoError(t, err)
		assert.Empty(t, entries, "probe file must be removed")
	})

	t.Run("unsupported storage backend", func(t *testing.T) {
		err := checkLocalLFSStorage(lfsx.Storage("s3"), t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported LFS storage backend")
	})

	t.Run("unwritable objects path", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
		objectsPath := filepath.Join(blocker, "lfs-objects")
		err := checkLocalLFSStorage(lfsx.StorageLocal, objectsPath)
		require.Error(t, err)
	})
}

func TestInvalidCloneAddrMessage(t *testing.T) {
	assert.Equal(t,
		"The clone address is not a valid URL.",
		invalidCloneAddrMessage(database.ErrInvalidCloneAddr{IsURLError: true}),
	)
	assert.Equal(t,
		"You are not allowed to import local repositories.",
		invalidCloneAddrMessage(database.ErrInvalidCloneAddr{IsPermissionDenied: true}),
	)
	assert.Equal(t,
		"The local path does not exist or is not a directory.",
		invalidCloneAddrMessage(database.ErrInvalidCloneAddr{IsInvalidPath: true}),
	)
	assert.Equal(t,
		"The clone address resolved to a local network address that is implicitly blocked.",
		invalidCloneAddrMessage(database.ErrInvalidCloneAddr{IsBlockedLocalAddress: true}),
	)
	assert.Equal(t,
		"The clone address is invalid.",
		invalidCloneAddrMessage(database.ErrInvalidCloneAddr{}),
	)
}
