/*
Copyright 2021 The Clusternet Authors.

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

package utils

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	appsapi "github.com/clusternet/clusternet/pkg/apis/apps/v1alpha1"
)

const (
	// UsernameKey is the key for username in the helm repo auth secret
	UsernameKey = "username"
	// PasswordKey is the key for password in the helm repo auth secret
	PasswordKey = "password"
)

var (
	Settings = cli.New()
)

// FindOCIChart will looks for an OCI-based helm chart from repository.
func FindOCIChart(chartRepo, chartName, chartVersion string) (bool, error) {
	// TODO: auth
	registryClient, err := registry.NewClient(
		registry.ClientOptDebug(Settings.Debug),
		registry.ClientOptWriter(os.Stdout),
		registry.ClientOptCredentialsFile(Settings.RegistryConfig),
	)
	if err != nil {
		return false, err
	}
	err = registryClient.WithResolver(true, false)
	if err != nil {
		return false, err
	}

	// Retrieve list of tags for repository
	ref := fmt.Sprintf("%s/%s", strings.TrimPrefix(chartRepo, fmt.Sprintf("%s://", registry.OCIScheme)), chartName)
	tags, err := registryClient.Tags(ref)
	if err != nil {
		return false, err
	}

	for _, tag := range tags {
		if tag == chartVersion {
			return true, nil
		}
	}
	return false, nil
}

// LocateAuthHelmChart will looks for a chart from auth repository and load it.
func LocateAuthHelmChart(cfg *action.Configuration, chartRepo, username, password, chartName, chartVersion string) (*chart.Chart, error) {
	client := action.NewInstall(cfg)
	client.ChartPathOptions.RepoURL = chartRepo
	client.ChartPathOptions.Version = chartVersion
	client.ChartPathOptions.Username = username
	client.ChartPathOptions.Password = password
	client.ChartPathOptions.InsecureSkipTLSverify = true
	// TODO: plainHTTP

	if registry.IsOCI(chartRepo) {
		/*oci based registries don't support to download index.yaml
		set RepoURL as an empty string to avoid downloading index.yaml
		in LocateChart() bellow
		*/
		client.ChartPathOptions.RepoURL = ""
		chartName = fmt.Sprintf("%s/%s", chartRepo, chartName)
		klog.V(5).Infof("oci based chart, full chart path is %s", chartName)
	}

	cp, err := client.ChartPathOptions.LocateChart(chartName, Settings)
	if err != nil {
		return nil, err
	}

	klog.V(5).Infof("chart %s/%s:%s locates at: %s", chartRepo, chartName, chartVersion, cp)

	// Check chart dependencies to make sure all are present in /charts
	chartRequested, err := loader.Load(cp)
	if err != nil {
		return nil, err
	}

	if err := CheckIfInstallable(chartRequested); err != nil {
		return nil, err
	}

	return chartRequested, nil
}

// CheckIfInstallable validates if a chart can be installed
// only application chart type is installable
func CheckIfInstallable(chart *chart.Chart) error {
	switch chart.Metadata.Type {
	case "", "application":
		return nil
	}
	return fmt.Errorf("chart %s is %s, which is not installable", chart.Name(), chart.Metadata.Type)
}

func InstallRelease(cfg *action.Configuration, hr *appsapi.HelmRelease,
	chart *chart.Chart, vals map[string]interface{}) (*release.Release, error) {
	client := action.NewInstall(cfg)
	client.ReleaseName = getReleaseName(hr)
	client.CreateNamespace = true
	client.Timeout = time.Minute * 5
	client.Namespace = hr.Spec.TargetNamespace

	return client.Run(chart, vals)
}

func UpgradeRelease(cfg *action.Configuration, hr *appsapi.HelmRelease,
	chart *chart.Chart, vals map[string]interface{}) (*release.Release, error) {
	klog.V(5).Infof("Upgrading HelmRelease %s", klog.KObj(hr))
	client := action.NewUpgrade(cfg)
	client.Timeout = time.Minute * 5
	client.Namespace = hr.Spec.TargetNamespace
	return client.Run(getReleaseName(hr), chart, vals)
}

func UninstallRelease(cfg *action.Configuration, hr *appsapi.HelmRelease) error {
	client := action.NewUninstall(cfg)
	client.Timeout = time.Minute * 5
	_, err := client.Run(getReleaseName(hr))
	if err != nil {
		if strings.Contains(err.Error(), "Release not loaded") {
			return nil
		}
		return err
	}
	return nil
}

func ReleaseNeedsUpgrade(rel *release.Release, hr *appsapi.HelmRelease, chart *chart.Chart, vals map[string]interface{}) bool {
	if rel.Name != getReleaseName(hr) {
		return true
	}
	if rel.Namespace != hr.Spec.TargetNamespace {
		return true
	}

	if rel.Chart.Metadata.Name != hr.Spec.Chart {
		return true
	}
	if rel.Chart.Metadata.Version != hr.Spec.ChartVersion {
		return true
	}

	if !reflect.DeepEqual(rel.Config, vals) {
		return true
	}

	return false
}

func UpdateRepo(repoURL string) error {
	klog.V(4).Infof("updating helm repo %s", repoURL)

	entry := repo.Entry{
		URL:                   repoURL,
		InsecureSkipTLSverify: true,
	}
	cr, err := repo.NewChartRepository(&entry, getter.All(Settings))
	if err != nil {
		return err
	}

	if _, err := cr.DownloadIndexFile(); err != nil {
		return err
	}

	klog.V(5).Infof("successfully got an repository update for %s", repoURL)
	return nil
}

type DeployContext struct {
	clientConfig             clientcmd.ClientConfig
	restConfig               *rest.Config
	cachedDiscoveryInterface discovery.CachedDiscoveryInterface
	restMapper               meta.RESTMapper
}

func NewDeployContext(config *clientcmdapi.Config) (*DeployContext, error) {
	clientConfig := clientcmd.NewDefaultClientConfig(*config, &clientcmd.ConfigOverrides{})
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("error while creating DeployContext: %v", err)
	}
	restConfig.QPS = 5
	restConfig.Burst = 10

	kubeclient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("error while creating DeployContext: %v", err)
	}

	discoveryClient := cacheddiscovery.NewMemCacheClient(kubeclient.Discovery())
	discoveryRESTMapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)

	dctx := &DeployContext{
		clientConfig:             clientConfig,
		restConfig:               restConfig,
		cachedDiscoveryInterface: discoveryClient,
		restMapper:               discoveryRESTMapper,
	}

	return dctx, nil
}

func (dctx *DeployContext) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return dctx.clientConfig
}

func (dctx *DeployContext) ToRESTConfig() (*rest.Config, error) {
	return dctx.restConfig, nil
}

func (dctx *DeployContext) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	return dctx.cachedDiscoveryInterface, nil
}

func (dctx *DeployContext) ToRESTMapper() (meta.RESTMapper, error) {
	return dctx.restMapper, nil
}

// GetHelmRepoCredentials get helm repo credentials from the given secret
func GetHelmRepoCredentials(kubeclient *kubernetes.Clientset, secretName, namespace string) (string, string, error) {
	secret, err := kubeclient.CoreV1().Secrets(namespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}

	username, ok := secret.Data[UsernameKey]
	if !ok {
		return "", "", fmt.Errorf("secret %s/%s does not contain username", namespace, secretName)
	}

	password, ok := secret.Data[PasswordKey]
	if !ok {
		return "", "", fmt.Errorf("secret %s/%s does not contain password", namespace, secretName)
	}

	return string(username), string(password), nil
}

// getReleaseName gets the release name from HelmRelease
func getReleaseName(hr *appsapi.HelmRelease) string {
	releaseName := hr.Name
	if hr.Spec.ReleaseName != nil {
		releaseName = *hr.Spec.ReleaseName
	}
	return releaseName
}
