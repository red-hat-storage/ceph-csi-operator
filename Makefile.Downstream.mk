# defining variables here before including "Makefile" makes these variables unique for current makefile
IMAGE_REGISTRY ?= quay.io
REGISTRY_NAMESPACE ?= ocs-dev
IMAGE_TAG ?= latest
IMAGE_NAME ?= cephcsi-operator

include Makefile

BUNDLE_IMAGE_NAME ?= $(IMAGE_NAME)-bundle

BUNDLE_IMG ?= $(IMAGE_REGISTRY)/$(REGISTRY_NAMESPACE)/$(BUNDLE_IMAGE_NAME):$(IMAGE_TAG)

# the PACKAGE_NAME is included in the bundle/CSV and is used in catalogsources
# for operators (like OperatorHub.io). Products that include the ceph-csi-operator
# bundle should use a different PACKAGE_NAME to prevent conflicts.
PACKAGE_NAME ?= cephcsi-operator

# Creating the New CatalogSource requires publishing CSVs that replace one operator,
# but can skip several. This can be accomplished using the skipRange annotation:
SKIP_RANGE ?=

# The default version of the bundle (CSV) can be found in
# config/manifests/bases/cephcsi-operator.clusterserviceversion.yaml
BUNDLE_VERSION ?= 4.18.0

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
DEFAULT_CHANNEL ?= alpha
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)

# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "preview,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=preview,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="preview,fast,stable")
CHANNELS ?= $(DEFAULT_CHANNEL)
BUNDLE_CHANNELS := --channels=$(CHANNELS)

OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk-$(OPERATOR_SDK_VERSION)
OPERATOR_SDK_VERSION ?= 1.34.1

NAME_PREFIX = ceph-csi-

SERVICE_ACCOUNTS := \
	$(NAME_PREFIX)cephfs-ctrlplugin-sa, \
	$(NAME_PREFIX)cephfs-nodeplugin-sa, \
	$(NAME_PREFIX)nfs-ctrlplugin-sa, \
	$(NAME_PREFIX)nfs-nodeplugin-sa, \
	$(NAME_PREFIX)rbd-ctrlplugin-sa, \
	$(NAME_PREFIX)rbd-nodeplugin-sa

# Remove spaces using tr
EXTRA_SERVICE_ACCOUNTS := $(shell echo $(SERVICE_ACCOUNTS) | tr -d ' ')

.PHONY: bundle
bundle: kustomize operator-sdk manifests
	rm -rf bundle
	rm -f config/manifests/kustomization.yaml
	mkdir -p build dist
	cd build && echo "$$BUILD_INSTALLER_OVERLAY" > kustomization.yaml
	cd build && $(KUSTOMIZE) edit add resource ../config/default/
	cd config/manifests/bases && $(KUSTOMIZE) edit add annotation --force 'olm.skipRange':"$(SKIP_RANGE)"
	cd config/manifests/bases && $(KUSTOMIZE) edit add patch --name cephcsi-operator.v0.1.1 --kind ClusterServiceVersion\
		--patch '[{"op": "replace", "path": "/spec/replaces", "value": "$(REPLACES)"}]'
	cd config/manifests && $(KUSTOMIZE) create --resources bases,../../build
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle \
		--overwrite --manifests --metadata --package $(PACKAGE_NAME) --version $(BUNDLE_VERSION) $(BUNDLE_METADATA_OPTS) \
		--extra-service-accounts $(EXTRA_SERVICE_ACCOUNTS)
	cd config/manifests/bases && yq -i 'del(.patches)' kustomization.yaml
	hack/update-csv-timestamp.sh
	rm -rf build

.PHONY: bundle-build
bundle-build: bundle ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push bundle image with the manager.
	$(CONTAINER_TOOL) push $(BUNDLE_IMG)

.PHONY: operator-sdk
operator-sdk: ## Download operator-sdk locally.
	@test -f $(OPERATOR_SDK) && echo "$(OPERATOR_SDK) already exists. Skipping download." && exit 0 ;\
	echo "Downloading $(OPERATOR_SDK)" ;\
        set -e ;\
        mkdir -p $(dir $(OPERATOR_SDK)) ;\
        OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
        curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/v${OPERATOR_SDK_VERSION}/operator-sdk_$${OS}_$${ARCH} ;\
        chmod +x $(OPERATOR_SDK)
