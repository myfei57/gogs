package v1

import (
	stdctx "context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	log "unknwon.dev/clog/v2"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/context"
	"gogs.io/gogs/internal/database"
	"gogs.io/gogs/internal/form"
	"gogs.io/gogs/internal/lfsx"
	"gogs.io/gogs/internal/route/api/v1/types"
)

func searchRepos(c *context.APIContext) {
	opts := &database.SearchRepoOptions{
		Keyword:  path.Base(c.Query("q")),
		OwnerID:  c.QueryInt64("uid"),
		PageSize: toAllowedPageSize(c.QueryInt("limit")),
		Page:     c.QueryInt("page"),
	}

	// Check visibility.
	if c.IsLogged && opts.OwnerID > 0 {
		if c.User.ID == opts.OwnerID {
			opts.Private = true
		} else {
			u, err := database.Handle.Users().GetByID(c.Req.Context(), opts.OwnerID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, map[string]any{
					"ok":    false,
					"error": err.Error(),
				})
				return
			}
			if u.IsOrganization() && u.IsOwnedBy(c.User.ID) {
				opts.Private = true
			}
			// FIXME: how about collaborators?
		}
	}

	repos, count, err := database.SearchRepositoryByName(opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	if err = database.RepositoryList(repos).LoadAttributes(); err != nil {
		c.JSON(http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	results := make([]*types.Repository, len(repos))
	for i := range repos {
		results[i] = toRepository(repos[i], nil)
	}

	c.SetLinkHeader(int(count), opts.PageSize)
	c.JSONSuccess(map[string]any{
		"ok":   true,
		"data": results,
	})
}

func listReposOfUser(c *context.APIContext, username string) {
	user, err := database.Handle.Users().GetByUsername(c.Req.Context(), username)
	if err != nil {
		c.NotFoundOrError(err, "get user by name")
		return
	}

	// Only list public repositories if user requests someone else's repository list,
	// or an organization isn't a member of.
	var ownRepos []*database.Repository
	if user.IsOrganization() {
		ownRepos, _, err = user.GetUserRepositories(c.User.ID, 1, user.NumRepos)
	} else {
		ownRepos, err = database.GetUserRepositories(&database.UserRepoOptions{
			UserID:   user.ID,
			Private:  c.User.ID == user.ID,
			Page:     1,
			PageSize: user.NumRepos,
		})
	}
	if err != nil {
		c.Error(err, "get user repositories")
		return
	}

	if err = database.RepositoryList(ownRepos).LoadAttributes(); err != nil {
		c.Error(err, "load attributes")
		return
	}

	// Early return for querying other user's repositories
	if c.User.ID != user.ID {
		repos := make([]*types.Repository, len(ownRepos))
		for i := range ownRepos {
			repos[i] = toRepository(ownRepos[i], &types.RepositoryPermission{Admin: true, Push: true, Pull: true})
		}
		c.JSONSuccess(&repos)
		return
	}

	accessibleReposWithAccessMode, err := database.Handle.Repositories().GetByCollaboratorIDWithAccessMode(c.Req.Context(), user.ID)
	if err != nil {
		c.Error(err, "get repositories accesses by collaborator")
		return
	}

	accessibleRepos := make([]*database.Repository, 0, len(accessibleReposWithAccessMode))
	for repo := range accessibleReposWithAccessMode {
		accessibleRepos = append(accessibleRepos, repo)
	}
	if err = database.RepositoryList(accessibleRepos).LoadAttributes(); err != nil {
		c.Error(err, "load attributes for accessible repositories")
		return
	}

	numOwnRepos := len(ownRepos)
	repos := make([]*types.Repository, 0, numOwnRepos+len(accessibleReposWithAccessMode))
	for _, r := range ownRepos {
		repos = append(repos, toRepository(r, &types.RepositoryPermission{Admin: true, Push: true, Pull: true}))
	}

	for repo, access := range accessibleReposWithAccessMode {
		repos = append(repos,
			toRepository(repo, &types.RepositoryPermission{
				Admin: access >= database.AccessModeAdmin,
				Push:  access >= database.AccessModeWrite,
				Pull:  true,
			}),
		)
	}

	c.JSONSuccess(&repos)
}

func listMyRepos(c *context.APIContext) {
	listReposOfUser(c, c.User.Name)
}

func listUserRepositories(c *context.APIContext) {
	listReposOfUser(c, c.Params(":username"))
}

func listOrgRepositories(c *context.APIContext) {
	listReposOfUser(c, c.Params(":org"))
}

type createRepoRequest struct {
	Name        string `json:"name" binding:"Required;AlphaDashDot;MaxSize(100)"`
	Description string `json:"description" binding:"MaxSize(255)"`
	Private     bool   `json:"private"`
	AutoInit    bool   `json:"auto_init"`
	Gitignores  string `json:"gitignores"`
	License     string `json:"license"`
	Readme      string `json:"readme"`
}

func createUserRepo(c *context.APIContext, owner *database.User, opt createRepoRequest) {
	repo, err := database.CreateRepository(c.User, owner, database.CreateRepoOptionsLegacy{
		Name:        opt.Name,
		Description: opt.Description,
		Gitignores:  opt.Gitignores,
		License:     opt.License,
		Readme:      opt.Readme,
		IsPrivate:   opt.Private,
		AutoInit:    opt.AutoInit,
	})
	if err != nil {
		if database.IsErrRepoAlreadyExist(err) ||
			database.IsErrNameNotAllowed(err) {
			c.ErrorStatus(http.StatusUnprocessableEntity, err)
		} else {
			if repo != nil {
				if err = database.DeleteRepository(c.User.ID, repo.ID); err != nil {
					log.Error("Failed to delete repository: %v", err)
				}
			}
			c.Error(err, "create repository")
		}
		return
	}

	c.JSON(201, toRepository(repo, &types.RepositoryPermission{Admin: true, Push: true, Pull: true}))
}

func createRepo(c *context.APIContext, opt createRepoRequest) {
	// Shouldn't reach this condition, but just in case.
	if c.User.IsOrganization() {
		c.ErrorStatus(http.StatusUnprocessableEntity, errors.New("Not allowed to create repository for organization."))
		return
	}
	createUserRepo(c, c.User, opt)
}

func createOrgRepo(c *context.APIContext, opt createRepoRequest) {
	org, err := database.GetOrgByName(c.Params(":org"))
	if err != nil {
		c.NotFoundOrError(err, "get organization by name")
		return
	}

	if !org.IsOwnedBy(c.User.ID) {
		c.ErrorStatus(http.StatusForbidden, errors.New("Given user is not owner of organization."))
		return
	}
	createUserRepo(c, org, opt)
}

func migrate(c *context.APIContext, f form.MigrateRepo) {
	ctxUser := c.User
	// Not equal means context user is an organization,
	// or is another user/organization if current user is admin.
	if f.UID != ctxUser.ID {
		org, err := database.Handle.Users().GetByID(c.Req.Context(), f.UID)
		if err != nil {
			if database.IsErrUserNotExist(err) {
				c.ErrorStatus(http.StatusUnprocessableEntity, err)
			} else {
				c.Error(err, "get user by ID")
			}
			return
		} else if !org.IsOrganization() && !c.User.IsAdmin {
			c.ErrorStatus(http.StatusForbidden, errors.New("Given user is not an organization."))
			return
		}
		ctxUser = org
	}

	if c.HasError() {
		c.ErrorStatus(http.StatusUnprocessableEntity, errors.New(c.GetErrMsg()))
		return
	}

	if ctxUser.IsOrganization() && !c.User.IsAdmin {
		// Check ownership of organization.
		if !ctxUser.IsOwnedBy(c.User.ID) {
			c.ErrorStatus(http.StatusForbidden, errors.New("Given user is not owner of organization."))
			return
		}
	}

	remoteAddr, err := form.ParseRemoteAddr(form.ParseRemoteAddrOptions{
		CloneAddr:    f.CloneAddr,
		User:         c.User,
		AuthUsername: f.AuthUsername,
		AuthPassword: f.AuthPassword,
	})
	if err != nil {
		if database.IsErrInvalidCloneAddr(err) {
			addrErr := err.(database.ErrInvalidCloneAddr)
			switch {
			case addrErr.IsURLError:
				c.ErrorStatus(http.StatusUnprocessableEntity, err)
			case addrErr.IsPermissionDenied:
				c.ErrorStatus(http.StatusUnprocessableEntity, errors.New("You are not allowed to import local repositories."))
			case addrErr.IsInvalidPath:
				c.ErrorStatus(http.StatusUnprocessableEntity, errors.New("Invalid local path, it does not exist or not a directory."))
			case addrErr.IsBlockedLocalAddress:
				c.ErrorStatus(http.StatusUnprocessableEntity, errors.New("Clone address resolved to a local network address that is implicitly blocked."))
			default:
				c.Error(err, "unexpected error")
			}
		} else {
			c.Error(err, "parse remote address")
		}
		return
	}

	repo, err := database.MigrateRepository(c.User, ctxUser, database.MigrateRepoOptions{
		Name:        f.RepoName,
		Description: f.Description,
		IsPrivate:   f.Private || conf.Repository.ForcePrivate,
		IsMirror:    f.Mirror,
		RemoteAddr:  remoteAddr,
	})
	if err != nil {
		if repo != nil {
			if errDelete := database.DeleteRepository(ctxUser.ID, repo.ID); errDelete != nil {
				log.Error("DeleteRepository: %v", errDelete)
			}
		}

		if database.IsErrReachLimitOfRepo(err) {
			c.ErrorStatus(http.StatusUnprocessableEntity, err)
		} else {
			c.Error(errors.New(database.HandleMirrorCredentials(err.Error(), true)), "migrate repository")
		}
		return
	}

	log.Trace("Repository migrated: %s/%s", ctxUser.Name, f.RepoName)
	c.JSON(201, toRepository(repo, &types.RepositoryPermission{Admin: true, Push: true, Pull: true}))
}

type precheckStepStatus string

const (
	precheckStepSuccess precheckStepStatus = "success"
	precheckStepFailed  precheckStepStatus = "failed"
	precheckStepWarning precheckStepStatus = "warning"
	precheckStepSkipped precheckStepStatus = "skipped"
)

type migratePrecheckStep struct {
	Name    string             `json:"name"`
	Status  precheckStepStatus `json:"status"`
	Message string             `json:"message"`
}

type migratePrecheckResponse struct {
	CanImport bool                  `json:"can_import"`
	Steps     []migratePrecheckStep `json:"steps"`
}

func appendPrecheckStep(steps []migratePrecheckStep, name string, status precheckStepStatus, message string) []migratePrecheckStep {
	return append(steps, migratePrecheckStep{
		Name:    name,
		Status:  status,
		Message: message,
	})
}

// precheckMigrate runs read-only validation for the repository migration
// request and returns step-by-step diagnostics. It does not create the
// repository nor write to the repository or hook tables.
func precheckMigrate(c *context.APIContext, f form.MigrateRepo) {
	ctxUser := c.User
	// Not equal means context user is an organization,
	// or is another user/organization if current user is admin.
	if f.UID != ctxUser.ID {
		org, err := database.Handle.Users().GetByID(c.Req.Context(), f.UID)
		if err != nil {
			if database.IsErrUserNotExist(err) {
				c.ErrorStatus(http.StatusUnprocessableEntity, err)
			} else {
				c.Error(err, "get user by ID")
			}
			return
		} else if !org.IsOrganization() && !c.User.IsAdmin {
			c.ErrorStatus(http.StatusForbidden, errors.New("Given user is not an organization."))
			return
		}
		ctxUser = org
	}

	if c.HasError() {
		c.ErrorStatus(http.StatusUnprocessableEntity, errors.New(c.GetErrMsg()))
		return
	}

	if ctxUser.IsOrganization() && !c.User.IsAdmin {
		// Check ownership of organization.
		if !ctxUser.IsOwnedBy(c.User.ID) {
			c.ErrorStatus(http.StatusForbidden, errors.New("Given user is not owner of organization."))
			return
		}
	}

	canImport := true
	steps := make([]migratePrecheckStep, 0, 4)

	// Step 1: Validate the clone address.
	remoteAddr, err := form.ParseRemoteAddr(form.ParseRemoteAddrOptions{
		CloneAddr:    f.CloneAddr,
		User:         c.User,
		AuthUsername: f.AuthUsername,
		AuthPassword: f.AuthPassword,
	})
	remoteAddrOK := err == nil
	if !remoteAddrOK {
		canImport = false
		steps = appendPrecheckStep(steps, "clone_address", precheckStepFailed, invalidCloneAddrMessage(err))
	} else {
		steps = appendPrecheckStep(steps, "clone_address", precheckStepSuccess, "The clone address is valid.")
	}

	// Step 2: Validate the repository name and check for conflicts.
	if nameErr := database.IsRepoNameAllowed(f.RepoName); nameErr != nil {
		canImport = false
		steps = appendPrecheckStep(steps, "repository_name", precheckStepFailed,
			fmt.Sprintf("Repository name %q is not allowed.", f.RepoName))
	} else {
		_, getErr := database.Handle.Repositories().GetByName(c.Req.Context(), ctxUser.ID, f.RepoName)
		switch {
		case getErr == nil:
			canImport = false
			steps = appendPrecheckStep(steps, "repository_name", precheckStepFailed,
				fmt.Sprintf("Repository name %q has already been taken.", f.RepoName))
		case database.IsErrRepoNotExist(getErr):
			steps = appendPrecheckStep(steps, "repository_name", precheckStepSuccess, "The repository name is available.")
		default:
			c.Error(getErr, "get repository by name")
			return
		}
	}

	// Step 3: Verify credentials and source repository accessibility.
	if !remoteAddrOK {
		steps = appendPrecheckStep(steps, "authentication", precheckStepSkipped,
			"Skipped because the clone address validation failed.")
	} else {
		timeout := time.Duration(conf.Git.Timeout.Migrate) * time.Second
		checkErr := database.CheckMigrateRemoteAddress(timeout, remoteAddr)
		switch {
		case checkErr == nil:
			steps = appendPrecheckStep(steps, "authentication", precheckStepSuccess,
				"The source repository is accessible with the provided credentials.")
		case database.IsErrMigrateRemoteAuthFailed(checkErr):
			canImport = false
			steps = appendPrecheckStep(steps, "authentication", precheckStepFailed, checkErr.Error())
		default:
			canImport = false
			steps = appendPrecheckStep(steps, "authentication", precheckStepFailed,
				"The source repository is not accessible: "+checkErr.Error())
		}
	}

	// Step 4: Check Git LFS capability, both locally and on the source server.
	status, message := precheckLFSStep(c.Req.Context(), remoteAddr, remoteAddrOK)
	if status == precheckStepFailed {
		canImport = false
	}
	steps = appendPrecheckStep(steps, "lfs", status, message)

	c.JSONSuccess(map[string]any{
		"ok":   true,
		"data": migratePrecheckResponse{CanImport: canImport, Steps: steps},
	})
}

func invalidCloneAddrMessage(err error) string {
	if !database.IsErrInvalidCloneAddr(err) {
		return "Failed to parse the clone address: " + err.Error()
	}
	addrErr := err.(database.ErrInvalidCloneAddr)
	switch {
	case addrErr.IsURLError:
		return "The clone address is not a valid URL."
	case addrErr.IsPermissionDenied:
		return "You are not allowed to import local repositories."
	case addrErr.IsInvalidPath:
		return "The local path does not exist or is not a directory."
	case addrErr.IsBlockedLocalAddress:
		return "The clone address resolved to a local network address that is implicitly blocked."
	default:
		return "The clone address is invalid."
	}
}

// checkLocalLFSStorage verifies the configured LFS storage backend is
// supported and the objects directory is writable. It creates a probe file
// under the objects directory and removes it, so it leaves no data behind.
// It performs no database writes.
func checkLocalLFSStorage(storage lfsx.Storage, objectsPath string) error {
	if storage != lfsx.StorageLocal {
		return errors.Newf("unsupported LFS storage backend %q, only %q is supported", storage, lfsx.StorageLocal)
	}

	if err := os.MkdirAll(objectsPath, os.ModePerm); err != nil {
		return errors.Wrap(err, "create LFS objects directory")
	}

	probe, err := os.CreateTemp(objectsPath, "precheck-*")
	if err != nil {
		return errors.Wrap(err, "write probe file to LFS objects directory")
	}
	probePath := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probePath)
	return nil
}

type remoteLFSStatus int

const (
	remoteLFSUnknown remoteLFSStatus = iota
	remoteLFSSupported
	remoteLFSUnsupported
)

const precheckLFSContentType = "application/vnd.git-lfs+json"

// lfsBatchEndpoint derives the Git LFS Batch API endpoint from the clone
// address. Credentials are extracted separately so they never appear in the
// endpoint URL or in error messages.
func lfsBatchEndpoint(remoteAddr string) (endpoint, username, password string, err error) {
	u, err := url.Parse(remoteAddr)
	if err != nil {
		return "", "", "", errors.Wrap(err, "parse remote address")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", "", errors.Newf("unsupported URL scheme %q", u.Scheme)
	}

	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
		u.User = nil
	}

	u.Path = strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(u.Path, ".git") {
		u.Path += ".git"
	}
	u.Path += "/info/lfs/objects/batch"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), username, password, nil
}

// probeRemoteLFS checks whether the source server serves the Git LFS Batch
// API by sending a minimal download request. A 404 or 410 response means LFS
// is unsupported, any other response means the endpoint is served (including
// 401 or 403, which indicate LFS is served but the request needs authorization),
// and a transport failure leaves the result undetermined.
func probeRemoteLFS(ctx stdctx.Context, remoteAddr string) (remoteLFSStatus, error) {
	endpoint, username, password, err := lfsBatchEndpoint(remoteAddr)
	if err != nil {
		return remoteLFSUnknown, err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		strings.NewReader(`{"operation":"download","transfers":["basic"],"objects":[]}`),
	)
	if err != nil {
		return remoteLFSUnknown, errors.Wrap(err, "create LFS batch request")
	}
	req.Header.Set("Accept", precheckLFSContentType)
	req.Header.Set("Content-Type", precheckLFSContentType)
	if username != "" {
		req.SetBasicAuth(username, password)
	}

	client := &http.Client{
		// Do not follow redirects to prevent a public URL from redirecting the
		// probe to an internal endpoint, matching the policy used by git.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return remoteLFSUnknown, errors.New("failed to connect to the Git LFS endpoint")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return remoteLFSUnsupported, nil
	default:
		return remoteLFSSupported, nil
	}
}

// precheckLFSStep checks the local LFS storage readiness and, when the source
// is an HTTP(S) repository, the source LFS capability.
func precheckLFSStep(reqCtx stdctx.Context, remoteAddr string, remoteAddrOK bool) (precheckStepStatus, string) {
	if err := checkLocalLFSStorage(lfsx.Storage(conf.LFS.Storage), conf.LFS.ObjectsPath); err != nil {
		return precheckStepFailed, "Git LFS is not available on this server: " + err.Error()
	}

	if !remoteAddrOK {
		return precheckStepWarning,
			"Local LFS storage is ready. Source LFS capability was not checked because the clone address validation failed."
	}

	endpoint, _, _, err := lfsBatchEndpoint(remoteAddr)
	if err != nil {
		// Non-HTTP sources (e.g. the git protocol or a local path) do not
		// expose the LFS Batch API.
		return precheckStepSuccess, "Local LFS storage is ready. The source does not use HTTP, so source LFS capability was not checked."
	}

	ctx, cancel := stdctx.WithTimeout(reqCtx, time.Duration(conf.Git.Timeout.Migrate)*time.Second)
	defer cancel()

	status, probeErr := probeRemoteLFS(ctx, remoteAddr)
	switch status {
	case remoteLFSSupported:
		return precheckStepSuccess, "The source server supports Git LFS, and local LFS storage is ready. Endpoint: " + endpoint
	case remoteLFSUnsupported:
		return precheckStepWarning, "The source server does not support Git LFS. LFS objects will not be available for the imported repository."
	default:
		log.Warn("Failed to probe Git LFS capability of %q: %v", endpoint, probeErr)
		return precheckStepWarning, "Unable to determine whether the source server supports Git LFS: " + probeErr.Error()
	}
}

// FIXME: inject in the handler chain
func parseOwnerAndRepo(c *context.APIContext) (*database.User, *database.Repository) {
	owner, err := database.Handle.Users().GetByUsername(c.Req.Context(), c.Params(":username"))
	if err != nil {
		if database.IsErrUserNotExist(err) {
			c.ErrorStatus(http.StatusUnprocessableEntity, err)
		} else {
			c.Error(err, "get user by name")
		}
		return nil, nil
	}

	repo, err := database.GetRepositoryByName(owner.ID, c.Params(":reponame"))
	if err != nil {
		c.NotFoundOrError(err, "get repository by name")
		return nil, nil
	}

	return owner, repo
}

func getRepo(c *context.APIContext) {
	_, repo := parseOwnerAndRepo(c)
	if c.Written() {
		return
	}

	c.JSONSuccess(toRepository(repo, &types.RepositoryPermission{
		Admin: c.Repo.IsAdmin(),
		Push:  c.Repo.IsWriter(),
		Pull:  true,
	}))
}

func deleteRepo(c *context.APIContext) {
	owner, repo := parseOwnerAndRepo(c)
	if c.Written() {
		return
	}

	if owner.IsOrganization() && !owner.IsOwnedBy(c.User.ID) {
		c.ErrorStatus(http.StatusForbidden, errors.New("Given user is not owner of organization."))
		return
	}

	if err := database.DeleteRepository(owner.ID, repo.ID); err != nil {
		c.Error(err, "delete repository")
		return
	}

	log.Trace("Repository deleted: %s/%s", owner.Name, repo.Name)
	c.NoContent()
}

func listForks(c *context.APIContext) {
	forks, err := c.Repo.Repository.GetForks()
	if err != nil {
		c.Error(err, "get forks")
		return
	}

	apiForks := make([]*types.Repository, len(forks))
	for i := range forks {
		if err := forks[i].GetOwner(); err != nil {
			c.Error(err, "get owner")
			return
		}

		accessMode := database.Handle.Permissions().AccessMode(
			c.Req.Context(),
			c.User.ID,
			forks[i].ID,
			database.AccessModeOptions{
				OwnerID: forks[i].OwnerID,
				Private: forks[i].IsPrivate,
			},
		)

		apiForks[i] = toRepository(forks[i],
			&types.RepositoryPermission{
				Admin: accessMode >= database.AccessModeAdmin,
				Push:  accessMode >= database.AccessModeWrite,
				Pull:  true,
			},
		)
	}

	c.JSONSuccess(&apiForks)
}

type editIssueTrackerRequest struct {
	EnableIssues          *bool   `json:"enable_issues"`
	EnableExternalTracker *bool   `json:"enable_external_tracker"`
	ExternalTrackerURL    *string `json:"external_tracker_url"`
	TrackerURLFormat      *string `json:"tracker_url_format"`
	TrackerIssueStyle     *string `json:"tracker_issue_style"`
}

func issueTracker(c *context.APIContext, form editIssueTrackerRequest) {
	_, repo := parseOwnerAndRepo(c)
	if c.Written() {
		return
	}

	if form.EnableIssues != nil {
		repo.EnableIssues = *form.EnableIssues
	}
	if form.EnableExternalTracker != nil {
		repo.EnableExternalTracker = *form.EnableExternalTracker
	}
	if form.ExternalTrackerURL != nil {
		repo.ExternalTrackerURL = *form.ExternalTrackerURL
	}
	if form.TrackerURLFormat != nil {
		repo.ExternalTrackerFormat = *form.TrackerURLFormat
	}
	if form.TrackerIssueStyle != nil {
		repo.ExternalTrackerStyle = *form.TrackerIssueStyle
	}

	if err := database.UpdateRepository(repo, false); err != nil {
		c.Error(err, "update repository")
		return
	}

	c.NoContent()
}

type editWikiRequest struct {
	EnableWiki         *bool   `json:"enable_wiki"`
	AllowPublicWiki    *bool   `json:"allow_public_wiki"`
	EnableExternalWiki *bool   `json:"enable_external_wiki"`
	ExternalWikiURL    *string `json:"external_wiki_url"`
}

func wiki(c *context.APIContext, form editWikiRequest) {
	_, repo := parseOwnerAndRepo(c)
	if c.Written() {
		return
	}

	if form.AllowPublicWiki != nil {
		repo.AllowPublicWiki = *form.AllowPublicWiki
	}
	if form.EnableExternalWiki != nil {
		repo.EnableExternalWiki = *form.EnableExternalWiki
	}
	if form.EnableWiki != nil {
		repo.EnableWiki = *form.EnableWiki
	}
	if form.ExternalWikiURL != nil {
		repo.ExternalWikiURL = *form.ExternalWikiURL
	}
	if err := database.UpdateRepository(repo, false); err != nil {
		c.Error(err, "update repository")
		return
	}

	c.NoContent()
}

func mirrorSync(c *context.APIContext) {
	_, repo := parseOwnerAndRepo(c)
	if c.Written() {
		return
	} else if !repo.IsMirror {
		c.NotFound()
		return
	}

	go database.MirrorQueue.Add(repo.ID)
	c.Status(http.StatusAccepted)
}

func releases(c *context.APIContext) {
	_, repo := parseOwnerAndRepo(c)
	releases, err := database.GetReleasesByRepoID(repo.ID)
	if err != nil {
		c.Error(err, "get releases by repository ID")
		return
	}
	apiReleases := make([]*types.RepositoryRelease, 0, len(releases))
	for _, r := range releases {
		publisher, err := database.Handle.Users().GetByID(c.Req.Context(), r.PublisherID)
		if err != nil {
			c.Error(err, "get release publisher")
			return
		}
		r.Publisher = publisher
	}
	for _, r := range releases {
		apiReleases = append(apiReleases, toRelease(r))
	}

	c.JSONSuccess(&apiReleases)
}
