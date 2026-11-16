//go:build live

package live

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// cleanup deletes, through GitHub's REST API, every package the run pushed.
// Each one is checked against the run prefix again before the call; a package
// outside it is never touched. Packages that can't be deleted are listed for
// the operator to remove by hand.
func (e *liveEnv) cleanup() {
	e.mu.Lock()

	repos := make([]string, 0, len(e.pushed))
	for repo := range e.pushed {
		repos = append(repos, repo)
	}

	e.mu.Unlock()

	sort.Strings(repos)

	for _, repo := range repos {
		if err := e.deletePackage(repo); err != nil {
			fmt.Fprintf(os.Stderr, "live cleanup: package of %s left behind, remove it by hand: %v\n", repo, err)
		}
	}
}

// deletePackage deletes the container package of repo, trying the owner as an
// organization first and as a user when that answers 404.
func (e *liveEnv) deletePackage(repo string) error {
	pkg, found := strings.CutPrefix(repo, e.owner+"/")
	if !e.inRunPrefix(repo) || !found || !strings.HasPrefix(pkg, strings.TrimPrefix(e.prefix, e.owner+"/")) {
		return fmt.Errorf("outside the run prefix, not deleted")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	status := 0

	for _, ownerType := range []string{"orgs", "users"} {
		escaped := ownerType + "/" + url.PathEscape(e.owner) + "/packages/container/" + url.PathEscape(pkg)

		target, err := url.Parse(apiURL + escaped)
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target.String(), nil)
		if err != nil {
			return err
		}

		req.Header.Set("Authorization", "Bearer "+e.pat)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := apiClient().Do(req)
		if err != nil {
			// The error carries the URL, never the request headers.
			return err
		}

		_ = resp.Body.Close()

		status = resp.StatusCode
		fmt.Fprintf(os.Stderr, "live cleanup: DELETE %s package of %s: %d\n", ownerType, repo, status)

		if status != http.StatusNotFound {
			break
		}
	}

	if status != http.StatusNoContent {
		return fmt.Errorf("REST API answered %d", status)
	}

	return nil
}

// apiClient talks to api.github.com only and never follows redirects, so the
// PAT goes to no other host.
func apiClient() *http.Client {
	return &http.Client{
		Timeout:       time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
