# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
# Source of truth for the operator semantic version. Every `make
# bundle` regenerates the CSV from this — sed-bumping the CSV file
# alone is insufficient because operator-sdk overwrites it on each
# build. Konflux's bundle pipeline runs `make bundle` so this is
# what ends up baked into the bundle image's CSV and the catalog's
# embedded olm.bundle.object content (which is what OLM reads when
# computing channel heads). Keep in sync with the image tag in
# .tekton/operator-image-on-push.yaml etc.
VERSION ?= 1.1.0

# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "candidate,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=candidate,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="candidate,fast,stable")
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
#
# For example, running 'make bundle-build bundle-push catalog-build catalog-push' will build and push both
# ai/agent-office-operator-bundle:$VERSION and ai/agent-office-operator-catalog:$VERSION.
IMAGE_TAG_BASE ?= quay-quay-quay-test.apps.salamander.aimlworkbench.com/deanpeterson/agent-office-operator

# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:v$(VERSION)

# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)

# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

# Set the Operator SDK version to use. By default, what is installed on the system is used.
# This is useful for CI or a project to utilize a specific version of the operator-sdk toolkit.
OPERATOR_SDK_VERSION ?= v1.42.2
# Image URL to use all building/pushing image targets
IMG ?= $(IMAGE_TAG_BASE):v$(VERSION)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= agent-office-operator-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name agent-office-operator-builder
	$(CONTAINER_TOOL) buildx use agent-office-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm agent-office-operator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.18.0
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.1.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

.PHONY: operator-sdk
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
operator-sdk: ## Download operator-sdk locally if necessary.
ifeq (,$(wildcard $(OPERATOR_SDK)))
ifeq (, $(shell which operator-sdk 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPERATOR_SDK)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$${OS}_$${ARCH} ;\
	chmod +x $(OPERATOR_SDK) ;\
	}
else
OPERATOR_SDK = $(shell which operator-sdk)
endif
endif

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate bundle manifests and metadata, then validate generated files.
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	$(OPERATOR_SDK) bundle validate ./bundle

.PHONY: catalog-fbc
catalog-fbc: bundle opm ## Regenerate the FBC catalog.yaml from the bundle.
	@echo "Rendering bundle/ -> catalog/agent-office-operator/catalog.yaml"
	@printf -- "---\n# File-Based Catalog (FBC) declarative config for the Agent Office Operator.\n#\n# Regenerate with \`make catalog-fbc\` after \`make bundle\`. The bundle\n# block below is rendered by \`opm render bundle/\` so all CSV / CRD\n# content is inlined as olm.bundle.object properties — that's what\n# package-server needs to extract channel/displayName/etc. without\n# pulling the bundle image at list time.\n\nschema: olm.package\nname: agent-office-operator\ndefaultChannel: alpha\ndescription: |\n  OLM-managed operator for the Governed Agent Platform on OpenShift. Owns\n  the AgentWorkstation and MemoryModule CRDs and ships a ConsolePlugin\n  that adds Memory Module / Agent Workstation tabs to the operator CSV\n  detail page in OpenShift Console.\n\n---\nschema: olm.channel\npackage: agent-office-operator\nname: alpha\nentries:\n  - name: agent-office-operator.v$(VERSION)\n\n---\n" > catalog/agent-office-operator/catalog.yaml
	@$(OPM) render bundle/ --output=yaml | sed 's|image: ""|image: $(BUNDLE_IMG)|' >> catalog/agent-office-operator/catalog.yaml
	@$(OPM) validate catalog/

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) docker-push IMG=$(BUNDLE_IMG)

.PHONY: opm
OPM = $(LOCALBIN)/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.55.0/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif

# A comma-separated list of bundle images (e.g. make catalog-build BUNDLE_IMGS=example.com/operator-bundle:v0.1.0,example.com/operator-bundle:v0.2.0).
# These images MUST exist in a registry and be pull-able.
BUNDLE_IMGS ?= $(BUNDLE_IMG)

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)

# Set CATALOG_BASE_IMG to an existing catalog image tag to add $BUNDLE_IMGS to that image.
ifneq ($(origin CATALOG_BASE_IMG), undefined)
FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG)
endif

# Build a catalog image by adding bundle images to an empty catalog using the operator package manager tool, 'opm'.
# This recipe invokes 'opm' in 'semver' bundle add mode. For more information on add modes, see:
# https://github.com/operator-framework/community-operators/blob/7f1438c/docs/packaging-operator.md#updating-your-existing-operator
.PHONY: catalog-build
catalog-build: opm ## Build a catalog image.
	$(OPM) index add --container-tool $(CONTAINER_TOOL) --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(FROM_INDEX_OPT)

# Push the catalog image.
.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	$(MAKE) docker-push IMG=$(CATALOG_IMG)

##@ Version bump

# bump-version: atomically update every place the operator/trainer version
# appears, then regenerate the bundle + catalog from the new source-of-truth.
#
# Why this target exists: we spent a brutal day discovering that sed-bumping
# individual YAML files leaves the Makefile VERSION variable behind, which
# causes `make bundle` to regenerate the bundle CSV at the OLD version,
# silently invalidating every "release". OLM reads the bundle's embedded CSV
# (not image tags) to pick channel heads, so the apparent upgrade never lands.
#
# Usage:
#   make bump-version VERSION=0.0.47
#
# Optionally bump the trainer image too in the same call:
#   make bump-version VERSION=0.0.47 TRAINER_VERSION=0.0.13
#
# What it touches (alphabetical):
#   - .tekton/operator-image-on-push.yaml      output-image tag
#   - .tekton/operator-bundle-on-push.yaml     output-image tag
#   - .tekton/operator-catalog-on-push.yaml    output-image tag
#   - .tekton/autoresearch-trainer-on-push.yaml (only if TRAINER_VERSION given)
#   - Makefile                                 VERSION ?= ...
#   - bundle/manifests/...clusterserviceversion.yaml  name, version, replaces
#   - catalog/agent-office-operator/catalog.yaml      channel entry + skipRange + bundle
#   - config/manager/kustomization.yaml         newTag
#   - internal/controller/autoresearchproject_controller.go (defaultTrainerImage)
#   - internal/controller/pipeline.yaml         trainer image ref
#   - scripts/autoresearch-pipeline/pipeline.yaml trainer image ref (mirror)
#
# After running, verify with `git diff` and `make verify-version` before committing.
# Then push — Konflux rebuilds bundle/catalog/operator all consistent.
.PHONY: bump-version
bump-version: ## Bump VERSION across every place it's referenced. Usage: make bump-version VERSION=0.0.47 [TRAINER_VERSION=0.0.13]
	@if [ -z "$(VERSION)" ]; then echo "ERROR: pass VERSION=x.y.z"; exit 1; fi
	@OLD_VERSION=$$(grep -E '^VERSION \?=' Makefile | head -1 | awk '{print $$3}'); \
	NEW_VERSION="$(VERSION)"; \
	if [ -z "$$OLD_VERSION" ]; then echo "ERROR: cannot find OLD VERSION in Makefile"; exit 1; fi; \
	echo "bumping OPERATOR $$OLD_VERSION -> $$NEW_VERSION"; \
	sed -i.bak "s|^VERSION ?= $$OLD_VERSION|VERSION ?= $$NEW_VERSION|" Makefile && rm Makefile.bak; \
	for f in config/manager/kustomization.yaml \
	         .tekton/operator-image-on-push.yaml \
	         .tekton/operator-bundle-on-push.yaml \
	         .tekton/operator-catalog-on-push.yaml \
	         internal/controller/autoresearchproject_controller.go; do \
	  if [ -f $$f ]; then \
	    sed -i.bak "s|v$$OLD_VERSION|v$$NEW_VERSION|g" $$f && rm $$f.bak; \
	  fi; \
	done; \
	sed -i.bak "s|v$$OLD_VERSION|v$$NEW_VERSION|g; \
	            s|version: $$OLD_VERSION|version: $$NEW_VERSION|g; \
	            s|>=0.0.1 <$$OLD_VERSION|>=0.0.1 <$$NEW_VERSION|g; \
	            s|replaces: agent-office-operator.v[0-9]\\.[0-9]\\.[0-9]\\+|replaces: agent-office-operator.v$$OLD_VERSION|g" \
	    bundle/manifests/agent-office-operator.clusterserviceversion.yaml && \
	  rm bundle/manifests/agent-office-operator.clusterserviceversion.yaml.bak; \
	sed -i.bak "s|v$$OLD_VERSION|v$$NEW_VERSION|g; \
	            s|>=0.0.1 <$$OLD_VERSION|>=0.0.1 <$$NEW_VERSION|g" \
	    catalog/agent-office-operator/catalog.yaml && \
	  rm catalog/agent-office-operator/catalog.yaml.bak
	@if [ -n "$(TRAINER_VERSION)" ]; then \
	  OLD_TRAINER=$$(grep -oE 'autoresearch-trainer:v[0-9]+\.[0-9]+\.[0-9]+' internal/controller/pipeline.yaml | head -1 | sed 's/.*://'); \
	  NEW_TRAINER="v$(TRAINER_VERSION)"; \
	  if [ -z "$$OLD_TRAINER" ]; then echo "ERROR: cannot find OLD TRAINER version in pipeline.yaml"; exit 1; fi; \
	  echo "bumping TRAINER $$OLD_TRAINER -> $$NEW_TRAINER"; \
	  for f in internal/controller/autoresearchproject_controller.go \
	           internal/controller/pipeline.yaml \
	           scripts/autoresearch-pipeline/pipeline.yaml \
	           .tekton/autoresearch-trainer-on-push.yaml; do \
	    if [ -f $$f ]; then \
	      sed -i.bak "s|autoresearch-trainer:$$OLD_TRAINER|autoresearch-trainer:$$NEW_TRAINER|g" $$f && rm $$f.bak; \
	    fi; \
	  done; \
	fi
	@$(MAKE) verify-version
	@echo ""
	@echo "==> Version bump complete. Next:"
	@echo "    1. git diff   (review what changed)"
	@echo "    2. make catalog-fbc-norebuild  (regen catalog.yaml inline content)"
	@echo "    3. git add -A && git commit && git push"
	@echo "    4. Also bump cluster/operator/catalogsource.yaml in agent-office repo"

# verify-version: sanity-check that every place referencing the operator
# version agrees. If they don't, the build chain will fail in confusing ways
# (the way it has been all day).
.PHONY: verify-version
verify-version: ## Sanity-check version consistency across all files.
	@MK=$$(grep -E '^VERSION \?=' Makefile | head -1 | awk '{print $$3}'); \
	echo "Makefile VERSION = $$MK"; \
	BAD=0; \
	check() { F=$$1; PATT=$$2; EXP=$$3; \
	  GOT=$$(grep -E "$$PATT" $$F 2>/dev/null | head -1); \
	  if echo "$$GOT" | grep -q "$$EXP"; then \
	    echo "  OK  $$F"; \
	  else \
	    echo "  ERR $$F (expected $$EXP, got: $$GOT)"; BAD=1; \
	  fi; }; \
	check bundle/manifests/agent-office-operator.clusterserviceversion.yaml "^  version:" "version: $$MK"; \
	check catalog/agent-office-operator/catalog.yaml "^  - name:" "v$$MK"; \
	check config/manager/kustomization.yaml "newTag" "v$$MK"; \
	check .tekton/operator-image-on-push.yaml "operator:v" "v$$MK"; \
	check .tekton/operator-bundle-on-push.yaml "operator-bundle:v" "v$$MK"; \
	check .tekton/operator-catalog-on-push.yaml "operator-catalog:v" "v$$MK"; \
	if [ $$BAD -ne 0 ]; then echo "VERSION MISMATCH — see ERR lines above"; exit 1; else echo "All version references consistent at v$$MK"; fi

# catalog-fbc-norebuild: regenerate catalog.yaml from bundle/ without
# the broken `make bundle` dependency. Used by bump-version to refresh
# the inline olm.bundle.object content after a Makefile version change.
#
# PREV_VERSION resolution:
#   1. PREV_VERSION arg if set (the only reliable source — bump-version
#      passes it explicitly because bump-version captures the version
#      BEFORE editing files).
#   2. Otherwise: read from git's last-committed catalog.yaml channel
#      entry (NOT the working copy, which may already reflect the new
#      version if bump-version ran first).
#   3. Otherwise: fail loudly. Better to fail than to silently produce
#      a self-replacing catalog entry (caught the v0.0.49 build with
#      "no channel head found in graph" after running unattended).
.PHONY: catalog-fbc-norebuild
catalog-fbc-norebuild: opm ## Regen catalog.yaml inline bundle content from current bundle/ dir (no operator-sdk regen).
	@VERSION=$$(grep -E '^VERSION \?=' Makefile | head -1 | awk '{print $$3}'); \
	if [ -n "$(PREV_VERSION)" ]; then \
	  PREV_VERSION=$(PREV_VERSION); \
	elif git rev-parse --git-dir >/dev/null 2>&1; then \
	  PREV_VERSION=$$(git show HEAD:catalog/agent-office-operator/catalog.yaml 2>/dev/null \
	                 | grep -oE 'agent-office-operator\.v[0-9]+\.[0-9]+\.[0-9]+' \
	                 | head -1 \
	                 | sed 's/agent-office-operator\.v//'); \
	else \
	  PREV_VERSION=""; \
	fi; \
	if [ -z "$$PREV_VERSION" ] || [ "$$PREV_VERSION" = "$$VERSION" ]; then \
	  echo "ERR catalog-fbc-norebuild: cannot resolve a distinct PREV_VERSION (got '$$PREV_VERSION' vs VERSION='$$VERSION')."; \
	  echo "    Pass it explicitly: make catalog-fbc-norebuild PREV_VERSION=0.0.X"; \
	  exit 1; \
	fi; \
	OLD_VERSION=$$PREV_VERSION; \
	printf -- "---\n# File-Based Catalog (FBC) declarative config for the Agent Office Operator.\n#\n# Regenerated by 'make catalog-fbc-norebuild' (skips the operator-sdk\n# bundle regen step that historically failed on CRD validation).\n# The bundle block below is rendered by 'opm render bundle/' so all\n# CSV / CRD content is inlined as olm.bundle.object properties.\n\nschema: olm.package\nname: agent-office-operator\ndefaultChannel: alpha\ndescription: |\n  OLM-managed operator for the Governed Agent Platform on OpenShift.\n\n---\nschema: olm.channel\npackage: agent-office-operator\nname: alpha\nentries:\n  - name: agent-office-operator.v$$VERSION\n    replaces: agent-office-operator.v$$PREV_VERSION\n    skipRange: \">=0.0.1 <$$VERSION\"\n\n---\n" > catalog/agent-office-operator/catalog.yaml; \
	$(OPM) render bundle/ --output=yaml | sed "s|image: \"\"|image: $(BUNDLE_IMG)|" >> catalog/agent-office-operator/catalog.yaml

# preflight: the single command the pre-commit hook (and CI, eventually)
# runs to catch the silent-failure modes that ate ~14 hours debugging
# adapter persistence. Add a check here EVERY time you discover a new
# way the cluster lied to you about a broken build.
#
# Current asserts:
#   1. verify-version — version refs across Makefile/CSV/catalog/kustomization/
#      tekton must all agree. Caught the v0.0.19 zombie that masqueraded as
#      v0.0.42 for hours because `make bundle` regenerated CSV at the stale
#      Makefile VERSION.
#   2. catalog/agent-office-operator/catalog.yaml MUST contain `schema: olm.channel`.
#      My hand-rolled awk-based regen dropped it once and opm rejected the
#      whole catalog with "bundle vX not found in any channel entries"
#      (failure mode far from root cause).
#   3. internal/controller/pipeline.yaml MUST reference the SAME trainer
#      image tag as scripts/autoresearch-pipeline/pipeline.yaml AND as
#      defaultTrainerImage in autoresearchproject_controller.go. Drift
#      here means new pipeline gets uploaded but pods still run an old
#      trainer (e.g. v0.0.12 keeping silent-skip persistence behavior
#      after v0.0.13 fixed it).
.PHONY: preflight
preflight: verify-version preflight-unit-tests ## Run all structural sanity checks AND fast unit tests before commit.
	@BAD=0; \
	if ! grep -q '^schema: olm.channel' catalog/agent-office-operator/catalog.yaml; then \
	  echo "  ERR catalog/agent-office-operator/catalog.yaml missing 'schema: olm.channel' block"; \
	  echo "      (run 'make catalog-fbc-norebuild' to regenerate)"; \
	  BAD=1; \
	else \
	  echo "  OK  catalog.yaml has olm.channel block"; \
	fi; \
	ENTRY_NAME=$$(grep -oE 'name: agent-office-operator\.v[0-9]+\.[0-9]+\.[0-9]+' catalog/agent-office-operator/catalog.yaml | head -1 | awk '{print $$2}'); \
	REPLACES=$$(grep -oE 'replaces: agent-office-operator\.v[0-9]+\.[0-9]+\.[0-9]+' catalog/agent-office-operator/catalog.yaml | head -1 | awk '{print $$2}'); \
	if [ -n "$$ENTRY_NAME" ] && [ "$$ENTRY_NAME" = "$$REPLACES" ]; then \
	  echo "  ERR catalog.yaml channel entry replaces ITSELF ($$ENTRY_NAME). opm will fail with"; \
	  echo "      'no channel head found in graph' at build time. Edit the replaces: line OR"; \
	  echo "      re-run 'make catalog-fbc-norebuild PREV_VERSION=<previous version>'."; \
	  BAD=1; \
	else \
	  echo "  OK  channel entry $$ENTRY_NAME replaces $$REPLACES (distinct)"; \
	fi; \
	if grep -q "image: '{{TRAINER_IMAGE}}'" internal/controller/pipeline.yaml \
	   && grep -q "image: '{{TRAINER_IMAGE}}'" scripts/autoresearch-pipeline/pipeline.yaml; then \
	  echo "  OK  pipeline.yaml uses {{TRAINER_IMAGE}} placeholder (operator templates at upload)"; \
	  IMG_LINES=$$(grep -c "image: '{{TRAINER_IMAGE}}'" internal/controller/pipeline.yaml); \
	  if [ "$$IMG_LINES" != "1" ]; then \
	    echo "  ERR pipeline.yaml has $$IMG_LINES image: '{{TRAINER_IMAGE}}' lines (expected exactly 1)"; \
	    echo "      multi-image pipelines need ReplaceAll-safe substitution"; \
	    BAD=1; \
	  else \
	    echo "  OK  exactly one image: '{{TRAINER_IMAGE}}' line"; \
	  fi; \
	else \
	  IMG_PIPE=$$(grep -oE 'autoresearch-trainer:v[0-9]+\.[0-9]+\.[0-9]+' internal/controller/pipeline.yaml | head -1); \
	  IMG_SCRIPT=$$(grep -oE 'autoresearch-trainer:v[0-9]+\.[0-9]+\.[0-9]+' scripts/autoresearch-pipeline/pipeline.yaml | head -1); \
	  IMG_DEF=$$(grep -oE 'autoresearch-trainer:v[0-9]+\.[0-9]+\.[0-9]+' internal/controller/autoresearchproject_controller.go | head -1); \
	  if [ "$$IMG_PIPE" = "$$IMG_SCRIPT" ] && [ "$$IMG_PIPE" = "$$IMG_DEF" ] && [ -n "$$IMG_PIPE" ]; then \
	    echo "  OK  trainer image consistent at $$IMG_PIPE (untemplated mode)"; \
	  else \
	    echo "  ERR pipeline.yaml is neither templated NOR consistently pinned:"; \
	    echo "        pipeline.yaml         = $$IMG_PIPE"; \
	    echo "        scripts/.../pipeline  = $$IMG_SCRIPT"; \
	    echo "        defaultTrainerImage   = $$IMG_DEF"; \
	    BAD=1; \
	  fi; \
	fi; \
	if [ $$BAD -ne 0 ]; then echo "PREFLIGHT FAIL — see ERR lines above"; exit 1; else echo "All preflight checks passed"; fi

# preflight-unit-tests: run ONLY the fast, no-cluster-needed unit tests
# that exercise the operator's render + helper functions. The full
# `make test` target depends on envtest + controller-gen which is slow
# and CI-only; preflight needs to stay snappy enough to run on every
# commit. Tests in this set are explicitly listed via -run regex so a
# future heavier test added to internal/controller doesn't bloat the
# pre-commit time.
#
# WHY THIS EXISTS: v0.0.49 shipped a renderer that produced
# `image: '{{TRAINER_IMAGE}}'` literally (1-replace hit a comment
# before the image: line). One full Konflux cycle to discover.
# TestRender_NoPlaceholderSurvives catches it in <400ms locally.
# Every new render-path bug should add its own assert here.
.PHONY: preflight-unit-tests
preflight-unit-tests: ## Run the fast unit tests preflight depends on.
	@echo "  -- go test (render + resolve + verifyAdapterArtifact tests)..."
	@go test ./internal/controller/ -run 'TestRender_|TestResolveTrainerImage_|TestParseBearerChallenge|TestVerifyAdapterArtifact_|TestParseAgentProposal|TestValidateQLoRAConfig|TestExtractJSONObject|TestRenderAutoresearch|TestMvToBackstageResource_|TestSanitizeName|TestRenderBackstageYAML_|TestCatalogModelToBackstageResource|TestFetchModelCatalogEntities' -count=1 >/dev/null \
	  && echo "  OK  render + verify + agent + KB-bootstrap + Backstage-catalog + helper unit tests" \
	  || { echo "  ERR unit tests failed; re-run with: go test ./internal/controller/ -v -run 'TestRender_|TestResolveTrainerImage_|TestParseBearerChallenge|TestVerifyAdapterArtifact_|TestParseAgentProposal|TestValidateQLoRAConfig|TestExtractJSONObject|TestRenderAutoresearch|TestMvToBackstageResource_|TestSanitizeName|TestRenderBackstageYAML_|TestCatalogModelToBackstageResource|TestFetchModelCatalogEntities'"; exit 1; }

# install-hooks: drop a pre-commit hook that refuses to commit when
# preflight fails. Symlink (not copy) so future edits to the script
# don't require re-installing. Idempotent.
.PHONY: install-hooks
install-hooks: ## Install git pre-commit hook that runs `make preflight` on every commit.
	@mkdir -p .git/hooks; \
	if [ -L .git/hooks/pre-commit ] || [ -f .git/hooks/pre-commit ]; then \
	  rm -f .git/hooks/pre-commit; \
	fi; \
	ln -s ../../hack/pre-commit.sh .git/hooks/pre-commit; \
	echo "installed .git/hooks/pre-commit -> hack/pre-commit.sh"

