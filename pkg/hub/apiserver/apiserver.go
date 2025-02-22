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

package apiserver

import (
	"time"

	crdclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	crdinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes"
	"k8s.io/controller-manager/pkg/clientbuilder"
	"k8s.io/klog/v2"
	aggregatorinformers "k8s.io/kube-aggregator/pkg/client/informers/externalversions"

	"github.com/clusternet/clusternet/pkg/apis/proxies"
	proxiesinstall "github.com/clusternet/clusternet/pkg/apis/proxies/install"
	"github.com/clusternet/clusternet/pkg/exchanger"
	"github.com/clusternet/clusternet/pkg/features"
	clusternet "github.com/clusternet/clusternet/pkg/generated/clientset/versioned"
	informers "github.com/clusternet/clusternet/pkg/generated/informers/externalversions"
	shadowapiserver "github.com/clusternet/clusternet/pkg/hub/apiserver/shadow"
	socketstorage "github.com/clusternet/clusternet/pkg/registry/proxies/socket"
	"github.com/clusternet/clusternet/pkg/registry/proxies/socket/subresources"
)

var (
	// Scheme defines methods for serializing and deserializing API objects.
	Scheme = runtime.NewScheme()
	// Codecs provides methods for retrieving codecs and serializers for specific
	// versions and content types.
	Codecs = serializer.NewCodecFactory(Scheme)
	// ParameterCodec handles versioning of objects that are converted to query parameters.
	ParameterCodec = runtime.NewParameterCodec(Scheme)
)

func init() {
	proxiesinstall.Install(Scheme)

	// we need to add the options to empty v1
	// TODO fix the server code to avoid this
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})

	// TODO: keep the generic API server from wanting this
	unversioned := schema.GroupVersion{Group: "", Version: "v1"}
	Scheme.AddUnversionedTypes(unversioned,
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)
}

// ExtraConfig holds custom apiserver config
type ExtraConfig struct {
	// Place you custom config here.
}

// Config defines the config for the apiserver
type Config struct {
	GenericConfig *genericapiserver.RecommendedConfig
	ExtraConfig   ExtraConfig
}

// HubAPIServer contains state for a master/api server.
type HubAPIServer struct {
	GenericAPIServer *genericapiserver.GenericAPIServer
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
	ExtraConfig   *ExtraConfig
}

// CompletedConfig embeds a private pointer that cannot be instantiated outside of this package.
type CompletedConfig struct {
	*completedConfig
}

// Complete fills in any fields not set that are required to have valid data. It's mutating the receiver.
func (cfg *Config) Complete() CompletedConfig {
	c := completedConfig{
		cfg.GenericConfig.Complete(),
		&cfg.ExtraConfig,
	}

	c.GenericConfig.Version = &version.Info{
		Major: "1",
		Minor: "0",
	}

	return CompletedConfig{&c}
}

// New returns a new instance of HubAPIServer from the given config.
func (c completedConfig) New(tunnelLogging, socketConnection bool, extraHeaderPrefixes []string,
	kubeclient *kubernetes.Clientset, clusternetclient *clusternet.Clientset,
	clusternetInformerFactory informers.SharedInformerFactory,
	aggregatorInformerFactory aggregatorinformers.SharedInformerFactory,
	clientBuilder clientbuilder.ControllerClientBuilder,
	reservedNamespace string) (*HubAPIServer, error) {
	genericServer, err := c.GenericConfig.New("clusternet-hub", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}

	s := &HubAPIServer{
		GenericAPIServer: genericServer,
	}

	proxiesAPIGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(proxies.GroupName, Scheme, ParameterCodec, Codecs)

	var ec *exchanger.Exchanger
	if socketConnection {
		ec = exchanger.NewExchanger(tunnelLogging, clusternetInformerFactory.Clusters().V1beta1().ManagedClusters().Lister())
	}

	proxiesv1alpha1storage := map[string]rest.Storage{}
	proxiesv1alpha1storage["sockets"] = socketstorage.NewREST(socketConnection, ec)
	proxiesv1alpha1storage["sockets/proxy"] = subresources.NewProxyREST(socketConnection, ec, extraHeaderPrefixes)
	proxiesAPIGroupInfo.VersionedResourcesStorageMap["v1alpha1"] = proxiesv1alpha1storage

	if err := s.GenericAPIServer.InstallAPIGroup(&proxiesAPIGroupInfo); err != nil {
		return nil, err
	}

	// let informers get registered before hook starts
	if utilfeature.DefaultFeatureGate.Enabled(features.ShadowAPI) {
		clusternetInformerFactory.Apps().V1alpha1().Manifests().Informer()
		aggregatorInformerFactory.Apiregistration().V1().APIServices().Informer()
	}

	s.GenericAPIServer.AddPostStartHookOrDie("start-clusternet-hub-shadowapis", func(context genericapiserver.PostStartHookContext) error {
		if s.GenericAPIServer != nil && utilfeature.DefaultFeatureGate.Enabled(features.ShadowAPI) {
			klog.Infof("install shadow apis...")
			crdInformerFactory := crdinformers.NewSharedInformerFactory(
				crdclientset.NewForConfigOrDie(clientBuilder.ConfigOrDie("crd-shared-informers")),
				5*time.Minute,
			)
			ss := shadowapiserver.NewShadowAPIServer(s.GenericAPIServer,
				c.GenericConfig.MaxRequestBodyBytes,
				c.GenericConfig.MinRequestTimeout,
				c.GenericConfig.AdmissionControl,
				kubeclient.RESTClient(),
				clusternetclient,
				clusternetInformerFactory.Apps().V1alpha1().Manifests().Lister(),
				aggregatorInformerFactory.Apiregistration().V1().APIServices().Lister(),
				crdInformerFactory,
				reservedNamespace)
			crdInformerFactory.Start(context.StopCh)
			return ss.InstallShadowAPIGroups(context.StopCh, kubeclient.DiscoveryClient)
		}

		select {
		case <-context.StopCh:
		}

		return nil
	})

	return s, nil
}
