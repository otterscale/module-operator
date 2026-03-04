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

	addonsv1alpha1 "github.com/otterscale/api/addons/v1alpha1"
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
func cloneRepository(ctx context.Context, c client.Client, kt *addonsv1alpha1.KustomizationTemplate, namespace string) (*gitCheckoutResult, error) {
	logger := log.FromContext(ctx)

	dir, err := os.MkdirTemp("", "module-git-*")
	if err != nil {
		return nil, &SourceFetchError{URL: kt.URL, Err: fmt.Errorf("creating temp dir: %w", err)}
	}

	cloneOpts := &git.CloneOptions{
		URL:      kt.URL,
		Progress: nil,
		Depth:    1,
	}

	if kt.SecretRef != nil {
		auth, authErr := readGitCredentials(ctx, c, kt.SecretRef.Name, namespace, kt.URL)
		if authErr != nil {
			os.RemoveAll(dir)
			return nil, &SourceFetchError{URL: kt.URL, Err: authErr}
		}
		cloneOpts.Auth = auth
	}

	ref := resolveGitReference(kt.Ref)
	if ref != "" {
		cloneOpts.ReferenceName = plumbing.ReferenceName(ref)
		cloneOpts.SingleBranch = true
	}

	repo, err := git.PlainCloneContext(ctx, dir, false, cloneOpts)
	if err != nil {
		os.RemoveAll(dir)
		return nil, &SourceFetchError{URL: kt.URL, Err: err}
	}

	if kt.Ref != nil && kt.Ref.Semver != "" {
		commitSHA, semErr := checkoutSemverTag(repo, kt.Ref.Semver)
		if semErr != nil {
			os.RemoveAll(dir)
			return nil, &SourceFetchError{URL: kt.URL, Err: semErr}
		}
		logger.Info("Git repository cloned", "url", kt.URL, "commit", commitSHA)
		return &gitCheckoutResult{Dir: dir, Commit: commitSHA}, nil
	}

	if kt.Ref != nil && kt.Ref.Commit != "" {
		wt, wtErr := repo.Worktree()
		if wtErr != nil {
			os.RemoveAll(dir)
			return nil, &SourceFetchError{URL: kt.URL, Err: wtErr}
		}
		if err := wt.Checkout(&git.CheckoutOptions{Hash: plumbing.NewHash(kt.Ref.Commit)}); err != nil {
			os.RemoveAll(dir)
			return nil, &SourceFetchError{URL: kt.URL, Err: fmt.Errorf("checkout commit %s: %w", kt.Ref.Commit, err)}
		}
		logger.Info("Git repository cloned", "url", kt.URL, "commit", kt.Ref.Commit)
		return &gitCheckoutResult{Dir: dir, Commit: kt.Ref.Commit}, nil
	}

	head, err := repo.Head()
	if err != nil {
		os.RemoveAll(dir)
		return nil, &SourceFetchError{URL: kt.URL, Err: err}
	}

	commitSHA := head.Hash().String()
	logger.Info("Git repository cloned", "url", kt.URL, "commit", commitSHA)
	return &gitCheckoutResult{Dir: dir, Commit: commitSHA}, nil
}

func resolveGitReference(ref *addonsv1alpha1.GitReference) string {
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

func readGitCredentials(ctx context.Context, c client.Client, secretName, namespace, repoURL string) (transport.AuthMethod, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Name: secretName, Namespace: namespace}
	if err := c.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("reading git credentials secret %q: %w", secretName, err)
	}

	if identity, ok := secret.Data["identity"]; ok {
		signer, err := ssh.ParsePrivateKey(identity)
		if err != nil {
			return nil, fmt.Errorf("parsing SSH key from secret %q: %w", secretName, err)
		}
		auth := &gitssh.PublicKeys{User: "git", Signer: signer}
		if knownHosts, ok := secret.Data["known_hosts"]; ok {
			tmpFile, tmpErr := os.CreateTemp("", "known-hosts-*")
			if tmpErr != nil {
				return nil, fmt.Errorf("creating temp known_hosts file: %w", tmpErr)
			}
			defer os.Remove(tmpFile.Name())
			if _, writeErr := tmpFile.Write(knownHosts); writeErr != nil {
				tmpFile.Close()
				return nil, fmt.Errorf("writing known_hosts to temp file: %w", writeErr)
			}
			tmpFile.Close()
			cb, cbErr := gitssh.NewKnownHostsCallback(tmpFile.Name())
			if cbErr != nil {
				return nil, fmt.Errorf("parsing known_hosts from secret %q: %w", secretName, cbErr)
			}
			auth.HostKeyCallback = cb
		}
		return auth, nil
	}

	if username, ok := secret.Data["username"]; ok {
		return &http.BasicAuth{
			Username: string(username),
			Password: string(secret.Data["password"]),
		}, nil
	}

	return nil, fmt.Errorf("secret %q contains neither SSH identity nor username/password", secretName)
}
