/*
Copyright 2026 The OtterScale Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package module

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/Masterminds/semver/v3"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
)

// SourceFetchError indicates a transient failure while cloning or
// pulling a Git repository.
type SourceFetchError struct {
	URL string
	Err error
}

func (e *SourceFetchError) Error() string {
	return fmt.Sprintf("failed to fetch source %q: %v", e.URL, e.Err)
}

func (e *SourceFetchError) Unwrap() error { return e.Err }

// gitCheckoutResult holds the directory path and commit SHA
// of a checked-out Git repository.
type gitCheckoutResult struct {
	Dir    string
	Commit string
}

// cloneRepository clones the Git repository described by the
// KustomizationTemplate into a temporary directory.
func cloneRepository(ctx context.Context, c client.Client, kt *modulev1alpha1.KustomizationTemplate, namespace string) (_ *gitCheckoutResult, err error) {
	logger := log.FromContext(ctx)

	dir, err := os.MkdirTemp("", "module-git-*")
	if err != nil {
		return nil, &SourceFetchError{URL: kt.URL, Err: fmt.Errorf("creating temp dir: %w", err)}
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()

	needsFullHistory := kt.Ref != nil && (kt.Ref.Semver != "" || kt.Ref.Commit != "")

	cloneOpts := &git.CloneOptions{
		URL:      kt.URL,
		Progress: nil,
	}
	if needsFullHistory {
		cloneOpts.Tags = git.AllTags
	} else {
		cloneOpts.Depth = 1
	}

	var authCleanup func()
	if kt.SecretRef != nil {
		auth, cleanup, authErr := readGitCredentials(ctx, c, kt.SecretRef.Name, namespace)
		if authErr != nil {
			return nil, &SourceFetchError{URL: kt.URL, Err: authErr}
		}
		authCleanup = cleanup
		cloneOpts.Auth = auth
	}
	defer func() {
		if authCleanup != nil {
			authCleanup()
		}
	}()

	ref := resolveGitReference(kt.Ref)
	if ref != "" {
		cloneOpts.ReferenceName = plumbing.ReferenceName(ref)
		cloneOpts.SingleBranch = true
	}

	repo, err := git.PlainCloneContext(ctx, dir, false, cloneOpts)
	if err != nil {
		return nil, &SourceFetchError{URL: kt.URL, Err: err}
	}

	if kt.Ref != nil && kt.Ref.Semver != "" {
		commitSHA, semErr := checkoutSemverTag(repo, kt.Ref.Semver)
		if semErr != nil {
			return nil, &SourceFetchError{URL: kt.URL, Err: semErr}
		}
		logger.Info("Git repository cloned", "url", kt.URL, "commit", commitSHA)
		return &gitCheckoutResult{Dir: dir, Commit: commitSHA}, nil
	}

	if kt.Ref != nil && kt.Ref.Commit != "" {
		wt, wtErr := repo.Worktree()
		if wtErr != nil {
			return nil, &SourceFetchError{URL: kt.URL, Err: wtErr}
		}
		if err = wt.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(kt.Ref.Commit)}); err != nil {
			return nil, &SourceFetchError{URL: kt.URL, Err: fmt.Errorf("checkout commit %s: %w", kt.Ref.Commit, err)}
		}
		logger.Info("Git repository cloned", "url", kt.URL, "commit", kt.Ref.Commit)
		return &gitCheckoutResult{Dir: dir, Commit: kt.Ref.Commit}, nil
	}

	head, err := repo.Head()
	if err != nil {
		return nil, &SourceFetchError{URL: kt.URL, Err: err}
	}

	commitSHA := head.Hash().String()
	logger.Info("Git repository cloned", "url", kt.URL, "commit", commitSHA)
	return &gitCheckoutResult{Dir: dir, Commit: commitSHA}, nil
}

func resolveGitReference(ref *modulev1alpha1.GitReference) string {
	if ref == nil {
		return ""
	}
	if ref.Tag != "" {
		return "refs/tags/" + ref.Tag
	}
	if ref.Branch != "" {
		return "refs/heads/" + ref.Branch
	}
	return ""
}

func checkoutSemverTag(repo *git.Repository, constraint string) (string, error) {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("invalid semver constraint %q: %w", constraint, err)
	}

	tags, err := repo.Tags()
	if err != nil {
		return "", fmt.Errorf("listing tags: %w", err)
	}

	var matched []*semver.Version
	tagMap := map[string]plumbing.Hash{}

	err = tags.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		v, parseErr := semver.NewVersion(name)
		if parseErr != nil {
			return nil
		}
		if c.Check(v) {
			matched = append(matched, v)
			tagMap[v.String()] = ref.Hash()
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matched) == 0 {
		return "", fmt.Errorf("no tags matching semver constraint %q", constraint)
	}

	sort.Sort(semver.Collection(matched))
	best := matched[len(matched)-1]
	hash := tagMap[best.String()]

	wt, err := repo.Worktree()
	if err != nil {
		return "", err
	}
	if err := wt.Checkout(&git.CheckoutOptions{Hash: hash}); err != nil {
		return "", fmt.Errorf("checkout tag %s: %w", best.Original(), err)
	}
	return hash.String(), nil
}

// readGitCredentials returns the auth method and a cleanup function that
// must be called after the auth is no longer needed (e.g. after clone).
// The cleanup function is nil when no temporary files were created.
func readGitCredentials(ctx context.Context, c client.Client, secretName, namespace string) (transport.AuthMethod, func(), error) {
	var secret corev1.Secret
	key := types.NamespacedName{Name: secretName, Namespace: namespace}
	if err := c.Get(ctx, key, &secret); err != nil {
		return nil, nil, fmt.Errorf("reading git credentials secret %q: %w", secretName, err)
	}

	if identity, ok := secret.Data["identity"]; ok {
		signer, err := ssh.ParsePrivateKey(identity)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing SSH key from secret %q: %w", secretName, err)
		}
		auth := &gitssh.PublicKeys{User: "git", Signer: signer}

		knownHosts, hasKnownHosts := secret.Data["known_hosts"]
		if !hasKnownHosts {
			return nil, nil, fmt.Errorf("secret %q contains SSH identity but is missing required known_hosts field", secretName)
		}

		tmpFile, tmpErr := os.CreateTemp("", "known-hosts-*")
		if tmpErr != nil {
			return nil, nil, fmt.Errorf("creating temp known_hosts file: %w", tmpErr)
		}
		tmpPath := tmpFile.Name()
		cleanup := func() { _ = os.Remove(tmpPath) }

		if _, writeErr := tmpFile.Write(knownHosts); writeErr != nil {
			_ = tmpFile.Close()
			cleanup()
			return nil, nil, fmt.Errorf("writing known_hosts to temp file: %w", writeErr)
		}
		if err := tmpFile.Close(); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("closing temp known_hosts file: %w", err)
		}
		cb, cbErr := gitssh.NewKnownHostsCallback(tmpPath)
		if cbErr != nil {
			cleanup()
			return nil, nil, fmt.Errorf("parsing known_hosts from secret %q: %w", secretName, cbErr)
		}
		auth.HostKeyCallback = cb
		return auth, cleanup, nil
	}

	if username, ok := secret.Data["username"]; ok {
		return &http.BasicAuth{
			Username: string(username),
			Password: string(secret.Data["password"]),
		}, nil, nil
	}

	return nil, nil, fmt.Errorf("secret %q contains neither SSH identity nor username/password", secretName)
}
