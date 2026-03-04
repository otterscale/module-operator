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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	addonsv1alpha1 "github.com/otterscale/api/addons/v1alpha1"
)

const (
	defaultHelmTimeout    = 5 * time.Minute
	defaultHelmMaxHistory = 10
)

// HelmReconcileResult contains the outcome of a Helm reconciliation cycle.
type HelmReconcileResult struct {
	ChartVersion   string
	Revision       int
	Status         string
	ValuesChecksum string
}

// ReconcileHelmChart ensures the Helm release matches the desired state
// described by the ModuleTemplate and Module overrides. It installs or
// upgrades the release as necessary and returns the observed result.
func ReconcileHelmChart(
	ctx context.Context,
	c client.Client,
	restCfg *rest.Config,
	m *addonsv1alpha1.Module,
	mt *addonsv1alpha1.ModuleTemplate,
	operatorVersion string,
) (*HelmReconcileResult, error) {
	if mt.Spec.HelmChart == nil {
		return nil, &TemplateInvalidError{
			Name:    mt.Name,
			Message: "helmChart spec is nil but Module expects a HelmChart template",
		}
	}

	ht := mt.Spec.HelmChart
	targetNS := TargetNamespace(m, mt)

	if ht.CreateNamespace {
		if err := EnsureNamespace(ctx, c, targetNS); err != nil {
			return nil, err
		}
	}

	ch, err := loadChart(ctx, c, ht, targetNS)
	if err != nil {
		return nil, err
	}

	vals, err := mergeValues(ht, m)
	if err != nil {
		return nil, &TemplateInvalidError{Name: mt.Name, Message: fmt.Sprintf("failed to merge values: %v", err)}
	}

	cfg, err := newHelmActionConfig(restCfg, targetNS)
	if err != nil {
		return nil, fmt.Errorf("creating Helm action config: %w", err)
	}

	releaseName := m.Name
	if ht.ReleaseName != "" {
		releaseName = ht.ReleaseName
	}
	timeout := defaultHelmTimeout
	if ht.Timeout != nil {
		timeout = ht.Timeout.Duration
	}
	maxHistory := defaultHelmMaxHistory
	if ht.MaxHistory != nil {
		maxHistory = *ht.MaxHistory
	}

	logger := log.FromContext(ctx)

	existing, err := action.NewHistory(cfg).Run(releaseName)
	if err != nil && err != driver.ErrReleaseNotFound {
		return nil, fmt.Errorf("checking release history: %w", err)
	}

	var rel *release.Release

	if len(existing) == 0 {
		install := action.NewInstall(cfg)
		install.ReleaseName = releaseName
		install.Namespace = targetNS
		install.CreateNamespace = ht.CreateNamespace
		install.Timeout = timeout
		install.Wait = true

		rel, err = install.RunWithContext(ctx, ch, vals)
		if err != nil {
			return nil, fmt.Errorf("helm install failed: %w", err)
		}
		logger.Info("Helm chart installed", "release", releaseName, "namespace", targetNS, "version", ch.Metadata.Version)
	} else {
		upgrade := action.NewUpgrade(cfg)
		upgrade.Namespace = targetNS
		upgrade.Timeout = timeout
		upgrade.Wait = true
		upgrade.MaxHistory = maxHistory
		if ht.Upgrade != nil {
			upgrade.Force = ht.Upgrade.Force
			upgrade.CleanupOnFail = ht.Upgrade.CleanupOnFail
		}

		rel, err = upgrade.RunWithContext(ctx, releaseName, ch, vals)
		if err != nil {
			if ht.Upgrade != nil && ht.Upgrade.EnableRollback {
				logger.Info("Upgrade failed, rolling back", "error", err)
				rollback := action.NewRollback(cfg)
				rollback.Wait = true
				rollback.Timeout = timeout
				if rbErr := rollback.Run(releaseName); rbErr != nil {
					logger.Error(rbErr, "Rollback also failed")
				}
			}
			return nil, fmt.Errorf("helm upgrade failed: %w", err)
		}
		logger.Info("Helm chart upgraded", "release", releaseName, "namespace", targetNS, "version", ch.Metadata.Version)
	}

	checksum := computeValuesChecksum(vals)

	return &HelmReconcileResult{
		ChartVersion:   ch.Metadata.Version,
		Revision:       rel.Version,
		Status:         string(rel.Info.Status),
		ValuesChecksum: checksum,
	}, nil
}

// UninstallHelmChart removes the Helm release from the cluster.
func UninstallHelmChart(ctx context.Context, restCfg *rest.Config, releaseName, namespace string) error {
	cfg, err := newHelmActionConfig(restCfg, namespace)
	if err != nil {
		return fmt.Errorf("creating Helm action config for uninstall: %w", err)
	}

	_, err = action.NewHistory(cfg).Run(releaseName)
	if err == driver.ErrReleaseNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking release history for uninstall: %w", err)
	}

	uninstall := action.NewUninstall(cfg)
	uninstall.Wait = true
	uninstall.Timeout = defaultHelmTimeout

	if _, err := uninstall.Run(releaseName); err != nil {
		return fmt.Errorf("helm uninstall failed: %w", err)
	}
	log.FromContext(ctx).Info("Helm release uninstalled", "release", releaseName, "namespace", namespace)
	return nil
}

func newHelmActionConfig(restCfg *rest.Config, namespace string) (*action.Configuration, error) {
	cfg := new(action.Configuration)
	getter := &restConfigGetter{cfg: restCfg, ns: namespace}
	if err := cfg.Init(getter, namespace, "secret", func(format string, v ...interface{}) {}); err != nil {
		return nil, err
	}
	return cfg, nil
}

func mergeValues(ht *addonsv1alpha1.HelmChartTemplate, m *addonsv1alpha1.Module) (map[string]interface{}, error) {
	base := map[string]interface{}{}
	if ht.Values != nil && ht.Values.Raw != nil {
		if err := json.Unmarshal(ht.Values.Raw, &base); err != nil {
			return nil, fmt.Errorf("unmarshalling template values: %w", err)
		}
	}
	if m.Spec.Values != nil && m.Spec.Values.Raw != nil {
		override := map[string]interface{}{}
		if err := json.Unmarshal(m.Spec.Values.Raw, &override); err != nil {
			return nil, fmt.Errorf("unmarshalling module override values: %w", err)
		}
		base = mergeMaps(base, override)
	}
	return base, nil
}

func mergeMaps(base, override map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(base))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		if baseMap, ok := result[k].(map[string]interface{}); ok {
			if overrideMap, ok := v.(map[string]interface{}); ok {
				result[k] = mergeMaps(baseMap, overrideMap)
				continue
			}
		}
		result[k] = v
	}
	return result
}

func computeValuesChecksum(vals map[string]interface{}) string {
	data, _ := json.Marshal(vals)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// restConfigGetter adapts a *rest.Config to the genericclioptions.RESTClientGetter
// interface required by Helm SDK's action.Configuration.Init.
type restConfigGetter struct {
	cfg *rest.Config
	ns  string
}

func (r *restConfigGetter) ToRESTConfig() (*rest.Config, error) {
	return r.cfg, nil
}

func (r *restConfigGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(r.cfg)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (r *restConfigGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := r.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(dc), nil
}

func (r *restConfigGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewDefaultClientConfig(
		*clientcmdapi.NewConfig(),
		&clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: r.ns}},
	)
}
