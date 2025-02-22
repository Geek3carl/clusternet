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

package localizer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	appsapi "github.com/clusternet/clusternet/pkg/apis/apps/v1alpha1"
	"github.com/clusternet/clusternet/pkg/controllers/apps/globalization"
	"github.com/clusternet/clusternet/pkg/controllers/apps/localization"
	clusternetclientset "github.com/clusternet/clusternet/pkg/generated/clientset/versioned"
	clusternetinformers "github.com/clusternet/clusternet/pkg/generated/informers/externalversions"
	applisters "github.com/clusternet/clusternet/pkg/generated/listers/apps/v1alpha1"
	"github.com/clusternet/clusternet/pkg/known"
	"github.com/clusternet/clusternet/pkg/utils"
)

var (
	chartKind = appsapi.SchemeGroupVersion.WithKind("HelmChart")
)

// Localizer defines configuration for the application localization
type Localizer struct {
	clusternetClient *clusternetclientset.Clientset

	locLister      applisters.LocalizationLister
	locSynced      cache.InformerSynced
	globLister     applisters.GlobalizationLister
	globSynced     cache.InformerSynced
	chartLister    applisters.HelmChartLister
	chartSynced    cache.InformerSynced
	manifestLister applisters.ManifestLister
	manifestSynced cache.InformerSynced

	locController  *localization.Controller
	globController *globalization.Controller

	chartCallback    func(*appsapi.HelmChart) error
	manifestCallback func(*appsapi.Manifest) error

	recorder record.EventRecorder

	// namespace where Manifests are created
	reservedNamespace string
}

func NewLocalizer(clusternetClient *clusternetclientset.Clientset,
	clusternetInformerFactory clusternetinformers.SharedInformerFactory,
	chartCallback func(*appsapi.HelmChart) error, manifestCallback func(*appsapi.Manifest) error,
	recorder record.EventRecorder, reservedNamespace string) (*Localizer, error) {

	localizer := &Localizer{
		clusternetClient:  clusternetClient,
		locLister:         clusternetInformerFactory.Apps().V1alpha1().Localizations().Lister(),
		locSynced:         clusternetInformerFactory.Apps().V1alpha1().Localizations().Informer().HasSynced,
		globLister:        clusternetInformerFactory.Apps().V1alpha1().Globalizations().Lister(),
		globSynced:        clusternetInformerFactory.Apps().V1alpha1().Globalizations().Informer().HasSynced,
		chartLister:       clusternetInformerFactory.Apps().V1alpha1().HelmCharts().Lister(),
		chartSynced:       clusternetInformerFactory.Apps().V1alpha1().HelmCharts().Informer().HasSynced,
		manifestLister:    clusternetInformerFactory.Apps().V1alpha1().Manifests().Lister(),
		manifestSynced:    clusternetInformerFactory.Apps().V1alpha1().Manifests().Informer().HasSynced,
		chartCallback:     chartCallback,
		manifestCallback:  manifestCallback,
		recorder:          recorder,
		reservedNamespace: reservedNamespace,
	}

	locController, err := localization.NewController(clusternetClient,
		clusternetInformerFactory.Apps().V1alpha1().Localizations(),
		clusternetInformerFactory.Apps().V1alpha1().HelmCharts(),
		clusternetInformerFactory.Apps().V1alpha1().Manifests(),
		recorder,
		localizer.handleLocalization,
		reservedNamespace)
	if err != nil {
		return nil, err
	}
	localizer.locController = locController

	globController, err := globalization.NewController(clusternetClient,
		clusternetInformerFactory.Apps().V1alpha1().Globalizations(),
		clusternetInformerFactory.Apps().V1alpha1().HelmCharts(),
		clusternetInformerFactory.Apps().V1alpha1().Manifests(),
		recorder,
		localizer.handleGlobalization,
		reservedNamespace)
	if err != nil {
		return nil, err
	}
	localizer.globController = globController

	return localizer, nil
}

func (l *Localizer) Run(workers int, stopCh <-chan struct{}) {
	klog.Info("starting Clusternet localizer ...")

	// Wait for the caches to be synced before starting workers
	if !cache.WaitForNamedCacheSync("localizer-controller",
		stopCh,
		l.locSynced,
		l.globSynced,
		l.chartSynced,
		l.manifestSynced,
	) {
		return
	}

	go l.locController.Run(workers, stopCh)
	go l.globController.Run(workers, stopCh)

	<-stopCh
}

func (l *Localizer) handleLocalization(loc *appsapi.Localization) error {
	switch loc.Spec.OverridePolicy {
	case appsapi.ApplyNow:
		klog.V(5).Infof("apply Localization %s now", klog.KObj(loc))
		if loc.Spec.Kind == chartKind.Kind {
			chart, err := l.chartLister.HelmCharts(loc.Spec.Namespace).Get(loc.Spec.Name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					klog.V(5).Infof("skipping apply Localization %s to not found %s", klog.KObj(loc), utils.FormatFeed(loc.Spec.Feed))
					break
				}
				return err
			}

			err = l.chartCallback(chart)
			if err != nil {
				return err
			}
			break
		}
		manifests, err := utils.ListManifestsBySelector(l.reservedNamespace, l.manifestLister, loc.Spec.Feed)
		if err != nil {
			return err
		}
		if manifests == nil {
			klog.V(5).Infof("skipping apply Localization %s to not found %s", klog.KObj(loc), utils.FormatFeed(loc.Spec.Feed))
			break
		}
		err = l.manifestCallback(manifests[0])
		if err != nil {
			return err
		}
	case appsapi.ApplyLater:
	default:
		msg := fmt.Sprintf("unsupported OverridePolicy %s", loc.Spec.OverridePolicy)
		l.recorder.Event(loc, corev1.EventTypeWarning, "InvalidOverridePolicy", msg)
		klog.ErrorDepth(2, msg)
		// will not sync such invalid objects again
		return nil
	}

	// remove finalizer
	if loc.DeletionTimestamp != nil {
		// remove finalizer
		locCopy := loc.DeepCopy()
		locCopy.Finalizers = utils.RemoveString(locCopy.Finalizers, known.AppFinalizer)
		_, err := l.clusternetClient.AppsV1alpha1().Localizations(locCopy.Namespace).Update(context.TODO(), locCopy, metav1.UpdateOptions{})
		if err != nil {
			klog.WarningDepth(4,
				fmt.Sprintf("failed to remove finalizer %s from Localization %s: %v", known.AppFinalizer, klog.KObj(locCopy), err))
			return err
		}
	}
	return nil
}

func (l *Localizer) handleGlobalization(glob *appsapi.Globalization) error {
	switch glob.Spec.OverridePolicy {
	case appsapi.ApplyNow:
		klog.V(5).Infof("apply Globalization %s now", klog.KObj(glob), appsapi.ApplyNow)
		if glob.Spec.Kind == chartKind.Kind {
			chart, err := l.chartLister.HelmCharts(glob.Spec.Namespace).Get(glob.Spec.Name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					klog.V(5).Infof("skipping apply Globalization %s to not found %s", klog.KObj(glob), utils.FormatFeed(glob.Spec.Feed))
					break
				}
				return err
			}

			err = l.chartCallback(chart)
			if err != nil {
				return err
			}
			break
		}
		manifests, err := utils.ListManifestsBySelector(l.reservedNamespace, l.manifestLister, glob.Spec.Feed)
		if err != nil {
			return err
		}
		if manifests == nil {
			klog.V(5).Infof("skipping apply Globalization %s to not found %s", klog.KObj(glob), utils.FormatFeed(glob.Spec.Feed))
			break
		}
		err = l.manifestCallback(manifests[0])
		if err != nil {
			return err
		}
	case appsapi.ApplyLater:
	default:
		msg := fmt.Sprintf("unsupported OverridePolicy %s", glob.Spec.OverridePolicy)
		l.recorder.Event(glob, corev1.EventTypeWarning, "InvalidOverridePolicy", msg)
		klog.ErrorDepth(2, msg)
		// will not sync such invalid objects again until the spec gets changed
		return nil
	}

	if glob.DeletionTimestamp != nil {
		// remove finalizer
		globCopy := glob.DeepCopy()
		globCopy.Finalizers = utils.RemoveString(globCopy.Finalizers, known.AppFinalizer)
		_, err := l.clusternetClient.AppsV1alpha1().Globalizations().Update(context.TODO(), globCopy, metav1.UpdateOptions{})
		if err != nil {
			klog.WarningDepth(4,
				fmt.Sprintf("failed to remove finalizer %s from Globalization %s: %v", known.AppFinalizer, klog.KObj(globCopy), err))
		}
		return err
	}
	return nil
}

func (l *Localizer) ApplyOverridesToDescription(desc *appsapi.Description) error {
	var allErrs []error
	descCopy := desc.DeepCopy()
	switch descCopy.Spec.Deployer {
	case appsapi.DescriptionHelmDeployer:
		desc.Spec.Raw = make([][]byte, len(descCopy.Spec.Charts))
		for idx, chartRef := range descCopy.Spec.Charts {
			overrides, err := l.getOverrides(descCopy.Namespace, appsapi.Feed{
				Kind:       chartKind.Kind,
				APIVersion: chartKind.Version,
				Namespace:  chartRef.Namespace,
				Name:       chartRef.Name,
			})
			if err != nil {
				allErrs = append(allErrs, err)
				continue
			}

			// use a whitespace explicitly
			result, err := applyOverrides([]byte(" "), overrides)
			if err != nil {
				allErrs = append(allErrs, err)
				continue
			}
			desc.Spec.Raw[idx] = result
		}
		return utilerrors.NewAggregate(allErrs)
	case appsapi.DescriptionGenericDeployer:
		for idx, rawObject := range descCopy.Spec.Raw {
			obj := &unstructured.Unstructured{}
			if err := json.Unmarshal(rawObject, obj); err != nil {
				allErrs = append(allErrs, err)
				continue
			}

			overrides, err := l.getOverrides(descCopy.Namespace, appsapi.Feed{
				Kind:       obj.GetKind(),
				APIVersion: obj.GetAPIVersion(),
				Namespace:  obj.GetNamespace(),
				Name:       obj.GetName(),
			})
			if err != nil {
				allErrs = append(allErrs, err)
				continue
			}

			result, err := applyOverrides(rawObject, overrides)
			if err != nil {
				allErrs = append(allErrs, err)
				continue
			}
			desc.Spec.Raw[idx] = result
		}
		return utilerrors.NewAggregate(allErrs)
	default:
		return fmt.Errorf("unsupported deployer %s", descCopy.Spec.Deployer)
	}
}

func (l *Localizer) getOverrides(namespace string, feed appsapi.Feed) ([]appsapi.OverrideConfig, error) {
	var uid types.UID
	switch feed.Kind {
	case chartKind.Kind:
		chart, err := l.chartLister.HelmCharts(feed.Namespace).Get(feed.Name)
		if err != nil {
			return nil, err
		}
		uid = chart.UID
	default:
		manifests, err := utils.ListManifestsBySelector(l.reservedNamespace, l.manifestLister, feed)
		if err != nil {
			return nil, err
		}
		if manifests == nil {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: feed.Kind}, feed.Name)
		}
		uid = manifests[0].UID
	}

	globs, err := l.globLister.List(labels.SelectorFromSet(labels.Set{
		string(uid): feed.Kind,
	}))
	if err != nil {
		return nil, err
	}
	sort.SliceStable(globs, func(i, j int) bool {
		if globs[i].Spec.Priority == globs[j].Spec.Priority {
			return globs[i].CreationTimestamp.Second() < globs[j].CreationTimestamp.Second()
		}

		return globs[i].Spec.Priority < globs[j].Spec.Priority
	})

	locs, err := l.locLister.Localizations(namespace).List(labels.SelectorFromSet(labels.Set{
		string(uid): feed.Kind,
	}))
	if err != nil {
		return nil, err
	}
	sort.SliceStable(locs, func(i, j int) bool {
		if locs[i].Spec.Priority == locs[j].Spec.Priority {
			return locs[i].CreationTimestamp.Second() < locs[j].CreationTimestamp.Second()
		}

		return locs[i].Spec.Priority < locs[j].Spec.Priority
	})

	var allOverrideConfigs []appsapi.OverrideConfig
	for _, glob := range globs {
		if glob.DeletionTimestamp != nil {
			continue
		}
		allOverrideConfigs = append(allOverrideConfigs, glob.Spec.Overrides...)
	}
	for _, loc := range locs {
		if loc.DeletionTimestamp != nil {
			continue
		}
		allOverrideConfigs = append(allOverrideConfigs, loc.Spec.Overrides...)
	}

	return allOverrideConfigs, nil
}
