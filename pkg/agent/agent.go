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

package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/klog/v2"
	utilpointer "k8s.io/utils/pointer"

	"github.com/clusternet/clusternet/pkg/agent/deployer"
	clusterapi "github.com/clusternet/clusternet/pkg/apis/clusters/v1beta1"
	"github.com/clusternet/clusternet/pkg/controllers/proxies/sockets"
	"github.com/clusternet/clusternet/pkg/features"
	clusternetclientset "github.com/clusternet/clusternet/pkg/generated/clientset/versioned"
	"github.com/clusternet/clusternet/pkg/known"
	"github.com/clusternet/clusternet/pkg/utils"
)

const (
	// default number of threads
	defaultThreadiness = 2
)

// Agent defines configuration for clusternet-agent
type Agent struct {
	ctx context.Context

	// Identity is the unique string identifying a lease holder across
	// all participants in an election.
	Identity string

	// ClusterID denotes current child cluster id
	ClusterID *types.UID

	// registrationOptions for cluster registration
	registrationOptions *ClusterRegistrationOptions

	// controllerOptions for leader election and client connection
	controllerOptions *utils.ControllerOptions

	// clientset for child cluster
	childKubeClientSet kubernetes.Interface

	// dedicated kubeconfig for accessing parent cluster, which is auto populated by the parent cluster
	// when cluster registration request gets approved
	parentDedicatedKubeConfig *rest.Config
	// dedicated namespace in parent cluster for current child cluster
	DedicatedNamespace *string

	// report cluster status
	statusManager *Manager

	deployer *deployer.Deployer
}

// NewAgent returns a new Agent.
func NewAgent(ctx context.Context, registrationOpts *ClusterRegistrationOptions, controllerOpts *utils.ControllerOptions) (*Agent, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("unable to get hostname: %v", err)
	}

	// add a uniquifier so that two processes on the same host don't accidentally both become active
	identity := hostname + "_" + string(uuid.NewUUID())
	klog.V(4).Infof("current identity lock id %q", identity)

	childKubeConfig, err := utils.LoadsKubeConfig(&controllerOpts.ClientConnection)
	if err != nil {
		return nil, err
	}
	// create clientset for child cluster
	childKubeClientSet := kubernetes.NewForConfigOrDie(childKubeConfig)

	agent := &Agent{
		ctx:                 ctx,
		Identity:            identity,
		childKubeClientSet:  childKubeClientSet,
		registrationOptions: registrationOpts,
		controllerOptions:   controllerOpts,
		statusManager: NewStatusManager(
			ctx,
			childKubeConfig.Host,
			controllerOpts.LeaderElection.ResourceNamespace,
			registrationOpts,
			childKubeClientSet,
		),
		deployer: deployer.NewDeployer(
			registrationOpts.ClusterSyncMode,
			childKubeConfig.Host,
			controllerOpts.LeaderElection.ResourceNamespace),
	}
	return agent, nil
}

func (agent *Agent) Run() error {
	klog.Info("starting agent controller ...")

	// if leader election is disabled, so runCommand inline until done.
	if !agent.controllerOptions.LeaderElection.LeaderElect {
		agent.run(agent.ctx)
		klog.Warning("finished without leader elect")
		return nil
	}

	// leader election is enabled, runCommand via LeaderElector until done and exit.
	curIdentity, err := utils.GenerateIdentity()
	if err != nil {
		return err
	}
	le, err := leaderelection.NewLeaderElector(*utils.NewLeaderElectionConfigWithDefaultValue(
		curIdentity,
		agent.controllerOptions.LeaderElection.ResourceName,
		agent.controllerOptions.LeaderElection.ResourceNamespace,
		agent.controllerOptions.LeaderElection.LeaseDuration.Duration,
		agent.controllerOptions.LeaderElection.RenewDeadline.Duration,
		agent.controllerOptions.LeaderElection.RetryPeriod.Duration,
		agent.childKubeClientSet,
		leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				agent.run(ctx)
			},
			OnStoppedLeading: func() {
				klog.Error("leader election got lost")
			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				if identity == curIdentity {
					// I just got the lock
					return
				}
				klog.Infof("new leader elected: %s", identity)
			},
		},
	))
	if err != nil {
		return err
	}
	le.Run(agent.ctx)
	return nil
}

func (agent *Agent) run(ctx context.Context) {
	agent.registerSelfCluster(ctx)

	// setup websocket connection
	if utilfeature.DefaultFeatureGate.Enabled(features.SocketConnection) {
		klog.Infof("featuregate %s is enabled, preparing setting up socket connection...", features.SocketConnection)
		socketConn, err := sockets.NewController(agent.parentDedicatedKubeConfig, agent.registrationOptions.TunnelLogging)
		if err != nil {
			klog.Exitf("failed to setup websocket connection: %v", err)

		}
		go socketConn.Run(ctx, agent.ClusterID)
	}

	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		agent.statusManager.Run(ctx, agent.parentDedicatedKubeConfig, agent.DedicatedNamespace, agent.ClusterID)
	}, time.Duration(0))

	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := agent.deployer.Run(ctx,
			agent.parentDedicatedKubeConfig,
			agent.childKubeClientSet,
			agent.DedicatedNamespace,
			agent.ClusterID,
			defaultThreadiness); err != nil {
			klog.Error(err)
		}
	}, time.Duration(0))

	<-ctx.Done()
}

// registerSelfCluster begins registering. It starts registering and blocked until the context is done.
func (agent *Agent) registerSelfCluster(ctx context.Context) {
	// complete your controller loop here
	klog.Info("start registering current cluster as a child cluster...")

	tryToUseSecret := true

	registerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	wait.JitterUntilWithContext(registerCtx, func(ctx context.Context) {
		// get cluster unique id
		if agent.ClusterID == nil {
			klog.Infof("retrieving cluster id")
			clusterID, err := agent.getClusterID(ctx, agent.childKubeClientSet)
			if err != nil {
				return
			}
			klog.Infof("current cluster id is %q", clusterID)
			agent.ClusterID = &clusterID
		}

		// get parent cluster kubeconfig
		if tryToUseSecret {
			secret, err := agent.childKubeClientSet.CoreV1().
				Secrets(agent.controllerOptions.LeaderElection.ResourceNamespace).
				Get(ctx, ParentClusterSecretName, metav1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				klog.Errorf("failed to get secretFromParentCluster: %v", err)
				return
			}
			if err == nil {
				klog.Infof("found existing secretFromParentCluster '%s/%s' that can be used to access parent cluster",
					agent.controllerOptions.LeaderElection.ResourceNamespace, ParentClusterSecretName)

				if string(secret.Data[known.ClusterAPIServerURLKey]) != agent.registrationOptions.ParentURL {
					klog.Warningf("the parent url got changed from %q to %q", secret.Data[known.ClusterAPIServerURLKey], agent.registrationOptions.ParentURL)
					klog.Warningf("will try to re-register current cluster")
				} else {
					parentDedicatedKubeConfig, err := utils.GenerateKubeConfigFromToken(agent.registrationOptions.ParentURL,
						string(secret.Data[corev1.ServiceAccountTokenKey]), secret.Data[corev1.ServiceAccountRootCAKey], 2)
					if err == nil {
						agent.parentDedicatedKubeConfig = parentDedicatedKubeConfig
					}
				}
			}
		}

		// bootstrap cluster registration
		if err := agent.bootstrapClusterRegistrationIfNeeded(ctx); err != nil {
			klog.Error(err)
			klog.Warning("something went wrong when using existing parent cluster credentials, switch to use bootstrap token instead")
			tryToUseSecret = false
			agent.parentDedicatedKubeConfig = nil
			return
		}

		// Cancel the context on success
		cancel()
	}, known.DefaultRetryPeriod, 0.3, true)
}

func (agent *Agent) getClusterID(ctx context.Context, childClientSet kubernetes.Interface) (types.UID, error) {
	lease, err := childClientSet.CoordinationV1().
		Leases(agent.controllerOptions.LeaderElection.ResourceNamespace).
		Get(ctx, agent.controllerOptions.LeaderElection.ResourceName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("unable to retrieve %s/%s Lease object: %v",
			agent.controllerOptions.LeaderElection.ResourceNamespace,
			agent.controllerOptions.LeaderElection.ResourceName, err)
		return "", err
	}
	return lease.UID, nil
}

func (agent *Agent) bootstrapClusterRegistrationIfNeeded(ctx context.Context) error {
	klog.Infof("try to bootstrap cluster registration if needed")

	clientConfig, err := agent.getBootstrapKubeConfigForParentCluster()
	if err != nil {
		return err
	}
	// create ClusterRegistrationRequest
	client := clusternetclientset.NewForConfigOrDie(clientConfig)
	crr, err := client.ClustersV1beta1().ClusterRegistrationRequests().Create(ctx,
		newClusterRegistrationRequest(*agent.ClusterID, agent.registrationOptions.ClusterType,
			generateClusterName(agent.registrationOptions.ClusterName, agent.registrationOptions.ClusterNamePrefix),
			agent.registrationOptions.ClusterSyncMode, agent.registrationOptions.ClusterLabels),
		metav1.CreateOptions{})

	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create ClusterRegistrationRequest: %v", err)
		}
		klog.Infof("a ClusterRegistrationRequest has already been created for cluster %q", *agent.ClusterID)
		// todo: update spec?
	} else {
		klog.Infof("successfully create ClusterRegistrationRequest %q", klog.KObj(crr))
	}

	// wait until stopCh is closed or request is approved
	err = agent.waitingForApproval(ctx, client)

	return err
}

func (agent *Agent) getBootstrapKubeConfigForParentCluster() (*rest.Config, error) {
	if agent.parentDedicatedKubeConfig != nil {
		return agent.parentDedicatedKubeConfig, nil
	}

	// todo: move to option.Validate() ?
	if len(agent.registrationOptions.ParentURL) == 0 {
		klog.Exitf("please specify a parent cluster url by flag --%s", ClusterRegistrationURL)
	}
	if len(agent.registrationOptions.BootstrapToken) == 0 {
		klog.Exitf("please specify a token for parent cluster accessing by flag --%s", ClusterRegistrationToken)
	}

	// get bootstrap kubeconfig from token
	clientConfig, err := utils.GenerateKubeConfigFromToken(agent.registrationOptions.ParentURL, agent.registrationOptions.BootstrapToken, nil, 1)
	if err != nil {
		return nil, fmt.Errorf("error while creating kubeconfig: %v", err)
	}

	return clientConfig, nil
}

func (agent *Agent) waitingForApproval(ctx context.Context, client clusternetclientset.Interface) error {
	var crr *clusterapi.ClusterRegistrationRequest
	var err error

	// wait until stopCh is closed or request is approved
	waitingCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wait.JitterUntilWithContext(waitingCtx, func(ctx context.Context) {
		crrName := generateClusterRegistrationRequestName(*agent.ClusterID)
		crr, err = client.ClustersV1beta1().ClusterRegistrationRequests().Get(ctx, crrName, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("failed to get ClusterRegistrationRequest %s: %v", crrName, err)
			return
		}
		if clusterName, ok := crr.Labels[known.ClusterNameLabel]; ok {
			agent.registrationOptions.ClusterName = clusterName
			klog.V(5).Infof("found existing cluster name %q, reuse it", clusterName)
		}

		if crr.Status.Result != nil && *crr.Status.Result == clusterapi.RequestApproved {
			klog.Infof("the registration request for cluster %q gets approved", *agent.ClusterID)
			// cancel on success
			cancel()
			return
		}

		klog.V(4).Infof("the registration request for cluster %q (%q) is still waiting for approval...",
			*agent.ClusterID, agent.registrationOptions.ClusterName)
	}, known.DefaultRetryPeriod, 0.4, true)

	parentDedicatedKubeConfig, err := utils.GenerateKubeConfigFromToken(agent.registrationOptions.ParentURL,
		string(crr.Status.DedicatedToken), crr.Status.CACertificate, 2)
	if err != nil {
		return err
	}
	agent.parentDedicatedKubeConfig = parentDedicatedKubeConfig
	agent.DedicatedNamespace = utilpointer.StringPtr(crr.Status.DedicatedNamespace)

	// once the request gets approved
	// store auto-populated credentials to Secret "parent-cluster" in "clusternet-system" namespace
	go agent.storeParentClusterCredentials(crr)

	return nil
}

func (agent *Agent) storeParentClusterCredentials(crr *clusterapi.ClusterRegistrationRequest) {
	klog.V(4).Infof("store parent cluster credentials to secret for later use")
	secretCtx, cancel := context.WithCancel(agent.ctx)
	defer cancel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: ParentClusterSecretName,
			Labels: map[string]string{
				known.ClusterBootstrappingLabel: known.CredentialsAuto,
				known.ClusterIDLabel:            string(*agent.ClusterID),
				known.ClusterNameLabel:          agent.registrationOptions.ClusterName,
			},
		},
		Data: map[string][]byte{
			corev1.ServiceAccountRootCAKey:    crr.Status.CACertificate,
			corev1.ServiceAccountTokenKey:     crr.Status.DedicatedToken,
			corev1.ServiceAccountNamespaceKey: []byte(crr.Status.DedicatedNamespace),
			known.ClusterAPIServerURLKey:      []byte(agent.registrationOptions.ParentURL),
		},
	}

	wait.JitterUntilWithContext(secretCtx, func(ctx context.Context) {
		_, err := agent.childKubeClientSet.CoreV1().
			Secrets(agent.controllerOptions.LeaderElection.ResourceNamespace).
			Create(ctx, secret, metav1.CreateOptions{})
		if err == nil {
			klog.V(5).Infof("successfully store parent cluster credentials")
			cancel()
			return
		}

		if apierrors.IsAlreadyExists(err) {
			klog.V(5).Infof("found existed parent cluster credentials, will try to update if needed")
			_, err = agent.childKubeClientSet.CoreV1().
				Secrets(agent.controllerOptions.LeaderElection.ResourceNamespace).
				Update(ctx, secret, metav1.UpdateOptions{})
			if err == nil {
				cancel()
				return
			}
		}
		klog.ErrorDepth(5, fmt.Sprintf("failed to store parent cluster credentials: %v", err))
	}, known.DefaultRetryPeriod, 0.4, true)
}

func newClusterRegistrationRequest(clusterID types.UID, clusterType, clusterName, clusterSyncMode, clusterLabels string) *clusterapi.ClusterRegistrationRequest {
	return &clusterapi.ClusterRegistrationRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: generateClusterRegistrationRequestName(clusterID),
			Labels: map[string]string{
				known.ClusterRegisteredByLabel: known.ClusternetAgentName,
				known.ClusterIDLabel:           string(clusterID),
				known.ClusterNameLabel:         clusterName,
			},
		},
		Spec: clusterapi.ClusterRegistrationRequestSpec{
			ClusterID:     clusterID,
			ClusterType:   clusterapi.ClusterType(clusterType),
			ClusterName:   clusterName,
			SyncMode:      clusterapi.ClusterSyncMode(clusterSyncMode),
			ClusterLabels: parseClusterLabels(clusterLabels),
		},
	}
}

func parseClusterLabels(clusterLabels string) map[string]string {
	if strings.TrimSpace(clusterLabels) == "" {
		return nil
	}
	clusterLabelsMap := make(map[string]string)
	clusterLabelsArray := strings.Split(clusterLabels, ",")
	for _, labelString := range clusterLabelsArray {
		labelArray := strings.Split(labelString, "=")
		if len(labelArray) != 2 {
			klog.Warningf("invalid cluster label %s", labelString)
			continue
		}
		clusterLabelsMap[labelArray[0]] = labelArray[1]
	}
	return clusterLabelsMap
}

func generateClusterRegistrationRequestName(clusterID types.UID) string {
	return fmt.Sprintf("%s%s", known.NamePrefixForClusternetObjects, string(clusterID))
}

func generateClusterName(clusterName, clusterNamePrefix string) string {
	if len(clusterName) == 0 {
		clusterName = fmt.Sprintf("%s-%s", clusterNamePrefix, utilrand.String(DefaultRandomUIDLength))
		klog.V(4).Infof("generate a random string %q as cluster name for later use", clusterName)
	}
	return clusterName
}
