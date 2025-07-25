# Copyright 2017 The Kubernetes Authors.
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

PKG = sigs.k8s.io/azuredisk-csi-driver
GIT_COMMIT ?= $(shell git rev-parse HEAD)
REGISTRY ?= andyzhangx
REGISTRY_NAME ?= $(shell echo $(REGISTRY) | sed "s/.azurecr.io//g")
IMAGE_NAME ?= azuredisk-csi
PLUGIN_NAME = azurediskplugin
IMAGE_VERSION ?= v1.34.0
CHART_VERSION ?= latest
CLOUD ?= AzurePublicCloud
# Use a custom version for E2E tests if we are testing in CI
ifdef CI
ifndef PUBLISH
override IMAGE_VERSION := $(IMAGE_VERSION)-$(GIT_COMMIT)
endif
endif
CSI_IMAGE_TAG ?= $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_VERSION)
CSI_IMAGE_TAG_LATEST = $(REGISTRY)/$(IMAGE_NAME):latest
REV = $(shell git describe --long --tags --dirty)
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
ENABLE_TOPOLOGY ?= false
LDFLAGS ?= "-X ${PKG}/pkg/azuredisk.driverVersion=${IMAGE_VERSION} -X ${PKG}/pkg/azuredisk.gitCommit=${GIT_COMMIT} -X ${PKG}/pkg/azuredisk.buildDate=${BUILD_DATE} -extldflags "-static"" ${GOTAGS}
E2E_HELM_OPTIONS ?= --set image.azuredisk.repository=$(REGISTRY)/$(IMAGE_NAME) --set image.azuredisk.tag=$(IMAGE_VERSION) --set image.azuredisk.pullPolicy=Always --set driver.userAgentSuffix="e2e-test" --set snapshot.VolumeSnapshotClass.enabled=true --set snapshot.enabled=true --set node.getNodeIDFromIMDS=true --set controller.runOnControlPlane=true --set controller.replicas=1 --set snapshot.snapshotController.replicas=1
E2E_HELM_OPTIONS += ${EXTRA_HELM_OPTIONS}
ifdef DISABLE_ZONE
E2E_HELM_OPTIONS += --set node.supportZone=false
endif
ifdef KUBERNETES_VERSION # disable KubeletRegistrationProbe on capz cluster testing
E2E_HELM_OPTIONS += --set linux.enableRegistrationProbe=false --set windows.enableRegistrationProbe=false
endif
GINKGO_FLAGS = -ginkgo.v -ginkgo.timeout=24h
ifeq ($(ENABLE_TOPOLOGY), true)
GINKGO_FLAGS += -ginkgo.focus="\[multi-az\]"
else
GINKGO_FLAGS += -ginkgo.focus="\[single-az\]"
endif
ifdef NODE_MACHINE_TYPE  # capz cluster
E2E_HELM_OPTIONS += --set controller.enableTrafficManager=true
endif
GOPATH ?= $(shell go env GOPATH)
GOBIN ?= $(GOPATH)/bin
GO111MODULE = on
DOCKER_CLI_EXPERIMENTAL = enabled
export GOPATH GOBIN GO111MODULE DOCKER_CLI_EXPERIMENTAL

# Generate all combination of all OS, ARCH, and OSVERSIONS for iteration
ALL_OS = linux windows
ALL_ARCH.linux = amd64 arm64
ALL_OS_ARCH.linux = $(foreach arch, ${ALL_ARCH.linux}, linux-$(arch))
ALL_ARCH.windows = amd64
ALL_OSVERSIONS.windows := 1809 ltsc2022
ALL_OS_ARCH.windows = $(foreach arch, $(ALL_ARCH.windows), $(foreach osversion, ${ALL_OSVERSIONS.windows}, windows-${osversion}-${arch}))
ALL_OS_ARCH = $(foreach os, $(ALL_OS), ${ALL_OS_ARCH.${os}})

# If set to true Windows containers will run as HostProcessContainers
WINDOWS_USE_HOST_PROCESS_CONTAINERS ?= false

# The current context of image building
# The architecture of the image
ARCH ?= amd64
# OS Version for the Windows images: 1809, ltsc2022
OSVERSION ?= 1809
# Output type of docker buildx build
OUTPUT_TYPE ?= registry

.PHONY: all
all: azuredisk

.PHONY: verify
verify: unit-test
	hack/verify-all.sh
	go vet ./pkg/...
	go build -o _output/${ARCH}/gen-disk-skus-map ./pkg/tool/

.PHONY: unit-test
unit-test: unit-test

.PHONY: unit-test
unit-test:
	go test -v -cover ./pkg/... ./test/utils/credentials

.PHONY: sanity-test
sanity-test: azuredisk
	go test -v -timeout=30m ./test/sanity

.PHONY: e2e-bootstrap
e2e-bootstrap: install-helm
ifdef WINDOWS_USE_HOST_PROCESS_CONTAINERS
	(docker pull $(CSI_IMAGE_TAG) && docker pull $(CSI_IMAGE_TAG)-windows-hp)  || make container-all push-manifest
else
	docker pull $(CSI_IMAGE_TAG) || make container-all push-manifest
endif
ifdef TEST_WINDOWS
	helm install azuredisk-csi-driver charts/${CHART_VERSION}/azuredisk-csi-driver --namespace kube-system --wait --timeout=15m -v=5 --debug \
		${E2E_HELM_OPTIONS} \
		--set windows.enabled=true \
		--set windows.useHostProcessContainers=${WINDOWS_USE_HOST_PROCESS_CONTAINERS} \
		--set linux.enabled=false \
		--set controller.replicas=1 \
		--set controller.logLevel=6 \
		--set node.logLevel=6 \
		--set snapshot.enabled=true \
		--set cloud=$(CLOUD)
else
	helm install azuredisk-csi-driver charts/${CHART_VERSION}/azuredisk-csi-driver --namespace kube-system --wait --timeout=15m -v=5 --debug \
		${E2E_HELM_OPTIONS} \
		--set snapshot.enabled=true \
		--set cloud=$(CLOUD)
endif

.PHONY: install-helm
install-helm:
	curl https://raw.githubusercontent.com/helm/helm/master/scripts/get-helm-3 | bash

.PHONY: e2e-teardown
e2e-teardown:
	helm delete azuredisk-csi-driver --namespace kube-system

.PHONY: azuredisk
azuredisk:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -a -ldflags ${LDFLAGS} -mod vendor -o _output/${ARCH}/${PLUGIN_NAME} ./pkg/azurediskplugin


.PHONY: azuredisk-windows
azuredisk-windows:
	CGO_ENABLED=0 GOOS=windows go build -a -ldflags ${LDFLAGS} -mod vendor -o _output/${ARCH}/${PLUGIN_NAME}.exe ./pkg/azurediskplugin

.PHONY: azuredisk-darwin
azuredisk-darwin:
	CGO_ENABLED=0 GOOS=darwin go build -a -ldflags ${LDFLAGS} -mod vendor -o _output/${ARCH}/${PLUGIN_NAME}.exe ./pkg/azurediskplugin

.PHONY: container
container: azuredisk
	docker build --no-cache -t $(CSI_IMAGE_TAG) --output=type=docker -f ./pkg/azurediskplugin/Dockerfile .

.PHONY: container-linux
container-linux:
	docker buildx build . \
		--pull \
		--output=type=$(OUTPUT_TYPE) \
		--tag $(CSI_IMAGE_TAG)-linux-$(ARCH) \
		--file ./pkg/azurediskplugin/Dockerfile \
		--platform="linux/$(ARCH)" \
		--build-arg ARCH=${ARCH} \
		--build-arg PLUGIN_NAME=${PLUGIN_NAME} \
		--provenance=false \
		--sbom=false

.PHONY: container-windows
container-windows:
	docker buildx build . \
		--pull \
		--output=type=$(OUTPUT_TYPE) \
		--platform="windows/$(ARCH)" \
		--tag $(CSI_IMAGE_TAG)-windows-$(OSVERSION)-$(ARCH) \
		--file ./pkg/azurediskplugin/Windows.Dockerfile \
		--build-arg ARCH=${ARCH} \
		--build-arg PLUGIN_NAME=${PLUGIN_NAME} \
		--build-arg OSVERSION=$(OSVERSION) \
		--provenance=false \
		--sbom=false
# workaround: only build hostprocess image once
ifdef WINDOWS_USE_HOST_PROCESS_CONTAINERS
ifeq ($(OSVERSION),ltsc2022)
	$(MAKE) container-windows-hostprocess
	$(MAKE) container-windows-hostprocess-latest
endif
endif

# Set --provenance=false to not generate the provenance (which is what causes the multi-platform index to be generated, even for a single platform).
.PHONY: container-windows-hostprocess
container-windows-hostprocess:
	docker buildx build --pull --output=type=$(OUTPUT_TYPE) --platform="windows/$(ARCH)" --provenance=false --sbom=false \
		-t $(CSI_IMAGE_TAG)-windows-hp -f ./pkg/azurediskplugin/WindowsHostProcess.Dockerfile .

.PHONY: container-windows-hostprocess-latest
container-windows-hostprocess-latest:
	docker buildx build --pull --output=type=$(OUTPUT_TYPE) --platform="windows/$(ARCH)" --provenance=false --sbom=false \
		-t $(CSI_IMAGE_TAG_LATEST)-windows-hp -f ./pkg/azurediskplugin/WindowsHostProcess.Dockerfile .

.PHONY: container-all
container-all: azuredisk-windows
	docker buildx rm container-builder || true
	docker buildx create --use --name=container-builder
ifeq ($(CLOUD), AzureStackCloud)
	docker run --privileged --name buildx_buildkit_container-builder0 -d --mount type=bind,src=/etc/ssl/certs,dst=/etc/ssl/certs moby/buildkit:latest || true
endif
	# enable qemu for arm64 build
	# https://github.com/docker/buildx/issues/464#issuecomment-741507760
	docker run --privileged --rm tonistiigi/binfmt --uninstall qemu-aarch64
	docker run --rm --privileged tonistiigi/binfmt --install all
	for arch in $(ALL_ARCH.linux); do \
		ARCH=$${arch} $(MAKE) azuredisk; \
		ARCH=$${arch} $(MAKE) container-linux; \
	done
	for osversion in $(ALL_OSVERSIONS.windows); do \
		OSVERSION=$${osversion} $(MAKE) container-windows; \
	done

.PHONY: push-manifest
push-manifest:
	docker manifest create --amend $(CSI_IMAGE_TAG) $(foreach osarch, $(ALL_OS_ARCH), $(CSI_IMAGE_TAG)-${osarch})
	# add "os.version" field to windows images (based on https://github.com/kubernetes/kubernetes/blob/master/build/pause/Makefile)
	set -x; \
	for arch in $(ALL_ARCH.windows); do \
		for osversion in $(ALL_OSVERSIONS.windows); do \
			BASEIMAGE=mcr.microsoft.com/windows/nanoserver:$${osversion}; \
			full_version=`docker manifest inspect $${BASEIMAGE} | jq -r '.manifests[0].platform["os.version"]'`; \
			docker manifest annotate --os windows --arch $${arch} --os-version $${full_version} $(CSI_IMAGE_TAG) $(CSI_IMAGE_TAG)-windows-$${osversion}-$${arch}; \
		done; \
	done
	docker manifest push --purge $(CSI_IMAGE_TAG)
	docker manifest inspect $(CSI_IMAGE_TAG)
ifdef PUBLISH
	docker manifest create --amend $(CSI_IMAGE_TAG_LATEST) $(foreach osarch, $(ALL_OS_ARCH), $(CSI_IMAGE_TAG)-${osarch})
	set -x; \
	for arch in $(ALL_ARCH.windows); do \
		for osversion in $(ALL_OSVERSIONS.windows); do \
			BASEIMAGE=mcr.microsoft.com/windows/nanoserver:$${osversion}; \
			full_version=`docker manifest inspect $${BASEIMAGE} | jq -r '.manifests[0].platform["os.version"]'`; \
			docker manifest annotate --os windows --arch $${arch} --os-version $${full_version} $(CSI_IMAGE_TAG_LATEST) $(CSI_IMAGE_TAG)-windows-$${osversion}-$${arch}; \
		done; \
	done
	docker manifest inspect $(CSI_IMAGE_TAG_LATEST)
	docker manifest create --amend $(CSI_IMAGE_TAG_LATEST)-windows-hp $(CSI_IMAGE_TAG_LATEST)-windows-hp
	docker manifest inspect $(CSI_IMAGE_TAG_LATEST)-windows-hp
endif

.PHONY: push-latest
push-latest:
ifdef CI
	docker manifest push --purge $(CSI_IMAGE_TAG_LATEST)
	docker manifest push --purge $(CSI_IMAGE_TAG_LATEST)-windows-hp
else
	docker push $(CSI_IMAGE_TAG_LATEST)
	docker push $(CSI_IMAGE_TAG_LATEST)-windows-hp
endif

.PHONY: clean
clean:
	go clean -r -x
	-rm -rf _output

.PHONY: create-metrics-svc
create-metrics-svc:
	kubectl create -f deploy/example/metrics/csi-azuredisk-controller-svc.yaml

.PHONY: delete-metrics-svc
delete-metrics-svc:
	kubectl delete -f deploy/example/metrics/csi-azuredisk-controller-svc.yaml --ignore-not-found

.PHONY: e2e-test
e2e-test:
	if [ ! -z "$(EXTERNAL_E2E_TEST)" ]; then \
		bash ./test/external-e2e/run.sh;\
	else \
		bash ./hack/parse-prow-creds.sh;\
		go test -v -timeout=0 ./test/e2e ${GINKGO_FLAGS};\
	fi
