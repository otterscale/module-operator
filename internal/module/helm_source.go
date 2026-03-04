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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
)

// ChartFetchError indicates a transient failure while downloading or
// loading a Helm chart from a repository.
type ChartFetchError struct {
	Chart   string
	RepoURL string
	Err     error
}

func (e *ChartFetchError) Error() string {
	return fmt.Sprintf("failed to fetch chart %q from %q: %v", e.Chart, e.RepoURL, e.Err)
}

func (e *ChartFetchError) Unwrap() error { return e.Err }

// loadChart downloads and loads a Helm chart based on the class's
// repository configuration. It supports both traditional Helm repositories
// and OCI registries. The returned cleanup function removes the temporary
// directory and must be called when the chart is no longer needed.
func loadChart(ctx context.Context, c client.Client, ht *modulev1alpha1.HelmChartTemplate, namespace string) (*chart.Chart, func(), error) {
	tmpDir, err := os.MkdirTemp("", "helm-chart-*")
	if err != nil {
		return nil, nil, &ChartFetchError{Chart: ht.Chart, RepoURL: ht.RepoURL, Err: fmt.Errorf("creating temp dir: %w", err)}
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	settings := cli.New()
	settings.RepositoryCache = tmpDir

	var username, password string
	if ht.SecretRef != nil {
		u, p, credErr := readRepoCredentials(ctx, c, ht.SecretRef.Name, namespace)
		if credErr != nil {
			cleanup()
			return nil, nil, &ChartFetchError{Chart: ht.Chart, RepoURL: ht.RepoURL, Err: credErr}
		}
		username, password = u, p
	}

	var ch *chart.Chart
	if strings.HasPrefix(ht.RepoURL, "oci://") {
		ch, err = loadOCIChart(settings, ht, username, password, tmpDir)
	} else {
		ch, err = loadRepoChart(settings, ht, username, password, tmpDir)
	}
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return ch, cleanup, nil
}

func loadRepoChart(settings *cli.EnvSettings, ht *modulev1alpha1.HelmChartTemplate, username, password, tmpDir string) (*chart.Chart, error) {
	pull := action.NewPullWithOpts(action.WithConfig(new(action.Configuration)))
	pull.RepoURL = ht.RepoURL
	pull.Settings = settings
	pull.DestDir = tmpDir
	pull.Untar = true
	pull.UntarDir = tmpDir
	if ht.Version != "" {
		pull.Version = ht.Version
	}
	if username != "" {
		pull.Username = username
		pull.Password = password
	}

	output, err := pull.Run(ht.Chart)
	if err != nil {
		return nil, &ChartFetchError{Chart: ht.Chart, RepoURL: ht.RepoURL, Err: fmt.Errorf("%s: %w", output, err)}
	}

	chartPath := fmt.Sprintf("%s/%s", tmpDir, ht.Chart)
	ch, err := loader.Load(chartPath)
	if err != nil {
		return nil, &ChartFetchError{Chart: ht.Chart, RepoURL: ht.RepoURL, Err: err}
	}
	return ch, nil
}

func loadOCIChart(settings *cli.EnvSettings, ht *modulev1alpha1.HelmChartTemplate, username, password, tmpDir string) (*chart.Chart, error) {
	var registryOpts []registry.ClientOption
	registryClient, err := registry.NewClient(registryOpts...)
	if err != nil {
		return nil, &ChartFetchError{Chart: ht.Chart, RepoURL: ht.RepoURL, Err: err}
	}

	if username != "" {
		ref := strings.TrimPrefix(ht.RepoURL, "oci://") + "/" + ht.Chart
		err = registryClient.Login(ref, registry.LoginOptBasicAuth(username, password))
		if err != nil {
			return nil, &ChartFetchError{Chart: ht.Chart, RepoURL: ht.RepoURL, Err: fmt.Errorf("OCI login failed: %w", err)}
		}
	}

	cfg := new(action.Configuration)
	cfg.RegistryClient = registryClient

	pull := action.NewPullWithOpts(action.WithConfig(cfg))
	pull.Settings = settings
	pull.DestDir = tmpDir
	pull.Untar = true
	pull.UntarDir = tmpDir
	if ht.Version != "" {
		pull.Version = ht.Version
	}

	chartRef := ht.RepoURL + "/" + ht.Chart
	output, err := pull.Run(chartRef)
	if err != nil {
		return nil, &ChartFetchError{Chart: ht.Chart, RepoURL: ht.RepoURL, Err: fmt.Errorf("%s: %w", output, err)}
	}

	chartPath := fmt.Sprintf("%s/%s", tmpDir, ht.Chart)
	ch, err := loader.Load(chartPath)
	if err != nil {
		return nil, &ChartFetchError{Chart: ht.Chart, RepoURL: ht.RepoURL, Err: err}
	}
	return ch, nil
}

func readRepoCredentials(ctx context.Context, c client.Client, secretName, namespace string) (username, password string, err error) {
	var secret corev1.Secret
	key := types.NamespacedName{Name: secretName, Namespace: namespace}
	if err := c.Get(ctx, key, &secret); err != nil {
		return "", "", fmt.Errorf("reading credentials secret %q: %w", secretName, err)
	}
	return string(secret.Data["username"]), string(secret.Data["password"]), nil
}
