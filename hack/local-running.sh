#!/usr/bin/env bash

# Copyright 2022 The Clusternet Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

KUBECONFIG_DIR=${KUBECONFIG_DIR:-"${HOME}/.kube/clusternet"}
KUBECONFIG_FILE=${KUBECONFIG_FILE:-"${HOME}/.kube/clusternet.config"}
PARENT_CLUSTER_NAME=${PARENT_CLUSTER_NAME:-"parent"}
CHILD_1_CLUSTER_NAME=${CHILD_1_CLUSTER_NAME:-"child1"}
CHILD_2_CLUSTER_NAME=${CHILD_2_CLUSTER_NAME:-"child2"}
CHILD_3_CLUSTER_NAME=${CHILD_3_CLUSTER_NAME:-"child3"}
KIND_IMAGE_VERSION=${KIND_IMAGE_VERSION:-"kindest/node:v1.22.0"}

function create_cluster() {
  local cluster_name=${1}
  local kubeconfig=${2}
  local image=${3}

  rm -f "${kubeconfig}"
  kind delete cluster --name="${cluster_name}" 2>&1
  kind create cluster --name "${cluster_name}" --kubeconfig="${kubeconfig}" --image="${image}" 2>&1

  kubectl config rename-context "kind-${cluster_name}" "${cluster_name}" --kubeconfig="${kubeconfig}"
  kind_server="https://$(docker inspect --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${cluster_name}-control-plane"):6443"

  kubectl --kubeconfig="${kubeconfig}" config set-cluster "kind-${cluster_name}" --server="${kind_server}"
  echo "Cluster ${cluster_name} has been initialized"
}

mkdir -p KUBECONFIG_DIR

create_cluster "${PARENT_CLUSTER_NAME}" "${KUBECONFIG_DIR}/${PARENT_CLUSTER_NAME}.config" "${KIND_IMAGE_VERSION}"
PARENT_CLUSTER_SERVER=${kind_server}

create_cluster "${CHILD_1_CLUSTER_NAME}" "${KUBECONFIG_DIR}/${CHILD_1_CLUSTER_NAME}.config" "${KIND_IMAGE_VERSION}"
create_cluster "${CHILD_2_CLUSTER_NAME}" "${KUBECONFIG_DIR}/${CHILD_2_CLUSTER_NAME}.config" "${KIND_IMAGE_VERSION}"
create_cluster "${CHILD_3_CLUSTER_NAME}" "${KUBECONFIG_DIR}/${CHILD_3_CLUSTER_NAME}.config" "${KIND_IMAGE_VERSION}"

export KUBECONFIG="${KUBECONFIG_DIR}/${PARENT_CLUSTER_NAME}.config:${KUBECONFIG_DIR}/${CHILD_1_CLUSTER_NAME}.config:${KUBECONFIG_DIR}/${CHILD_2_CLUSTER_NAME}.config:${KUBECONFIG_DIR}/${CHILD_3_CLUSTER_NAME}.config"
kubectl config view --flatten > "${KUBECONFIG_FILE}"
unset KUBECONFIG

echo "Updating helm repo..."
helm repo add clusternet https://clusternet.github.io/charts
helm repo update
echo "Updating helm finished"

echo "Installing clusternet-hub..."
helm --kubeconfig="${KUBECONFIG_FILE}" --kube-context="${PARENT_CLUSTER_NAME}" install \
  clusternet-hub -n clusternet-system --create-namespace clusternet/clusternet-hub
kubectl --kubeconfig="${KUBECONFIG_FILE}" --context="${PARENT_CLUSTER_NAME}" apply -f \
  https://raw.githubusercontent.com/clusternet/clusternet/main/manifests/samples/cluster_bootstrap_token.yaml
echo "Installing clusternet-hub finished"

echo "Installing clusternet-scheduler..."
helm --kubeconfig="${KUBECONFIG_FILE}" --kube-context="${PARENT_CLUSTER_NAME}" install \
  clusternet-scheduler -n clusternet-system --create-namespace clusternet/clusternet-scheduler
echo "Installing clusternet-scheduler finished"


echo "Installing clusternet-agent into child1..."
helm --kubeconfig="${KUBECONFIG_FILE}" --kube-context="${CHILD_1_CLUSTER_NAME}" install \
  clusternet-agent -n clusternet-system --create-namespace \
  --set parentURL="${PARENT_CLUSTER_SERVER}" \
  --set registrationToken=07401b.f395accd246ae52d \
  clusternet/clusternet-agent
echo "Installing clusternet-agent into child1 finished"

echo "Installing clusternet-agent into child2..."
helm --kubeconfig="${KUBECONFIG_FILE}" --kube-context="${CHILD_2_CLUSTER_NAME}" install \
  clusternet-agent -n clusternet-system --create-namespace \
  --set parentURL="${PARENT_CLUSTER_SERVER}" \
  --set registrationToken=07401b.f395accd246ae52d \
  clusternet/clusternet-agent
echo "Installing clusternet-agent into child2 finished"

echo "Installing clusternet-agent into child3..."
helm --kubeconfig="${KUBECONFIG_FILE}" --kube-context="${CHILD_3_CLUSTER_NAME}" install \
  clusternet-agent -n clusternet-system --create-namespace \
  --set parentURL="${PARENT_CLUSTER_SERVER}" \
  --set registrationToken=07401b.f395accd246ae52d \
  clusternet/clusternet-agent
echo "Installing clusternet-agent into child3 finished"

echo "Local clusternet is running now."
echo "To start using clusternet, please run:"
echo "  export KUBECONFIG=${KUBECONFIG_FILE}"
echo "  kubectl config get-contexts"
