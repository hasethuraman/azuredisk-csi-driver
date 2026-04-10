/*
Copyright 2024 The Kubernetes Authors.

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

package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

/*
When a node is terminated, persistent workflows using Azure disk volumes can take 6+ minutes to start up again.
This happens when a volume is not cleanly unmounted, which causes the Attach/Detach controller (in kube-controller-manager)
to wait for 6 minutes before issuing a force detach and allowing the volume to be attached to another node.

This PreStop lifecycle hook aims to ensure that before the node (and the CSI driver node pod running on it) is shut down,
all VolumeAttachment objects associated with that node are removed, thereby indicating that all volumes have been successfully unmounted and detached.

No unnecessary delay is added to the termination workflow, as the PreStop hook logic is only executed when the node is being drained
(thus preventing delays in termination where the node pod is killed due to a rolling restart, or during driver upgrades, but the workload pods are expected to be running).
If the PreStop hook hangs during its execution, the driver node pod will be forcefully terminated after terminationGracePeriodSeconds, defined in the pod spec.
*/

const clusterAutoscalerTaint = "ToBeDeletedByClusterAutoscaler"
const v1KarpenterTaint = "karpenter.sh/disrupted"
const v1beta1KarpenterTaint = "karpenter.sh/disruption"

// azureDiskCSIDriver is the attacher/driver name recorded in VolumeAttachment specs.
const azureDiskCSIDriver = "disk.csi.azure.com"

// kubeletCSIDir is the glob pattern for filesystem CSI volumes mounted on this node.
// Matches: /var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~csi/<pv-name>/
const kubeletCSIDir = "/var/lib/kubelet/pods/*/volumes/kubernetes.io~csi/*"

// kubeletCSIBlockDevicesDir is the glob pattern for raw-block CSI volumes mounted on this node.
// Matches: /var/lib/kubelet/pods/<uid>/volumeDevices/kubernetes.io~csi/<pv-name>/
const kubeletCSIBlockDevicesDir = "/var/lib/kubelet/pods/*/volumeDevices/kubernetes.io~csi/*"

// drainTaints includes taints used by K8s or autoscalers that signify node draining or pod eviction.
var drainTaints = map[string]struct{}{
	v1.TaintNodeUnschedulable: {}, // Kubernetes common eviction taint (kubectl drain)
	clusterAutoscalerTaint:    {},
	v1KarpenterTaint:          {},
	v1beta1KarpenterTaint:     {},
}

func PreStop(clientset kubernetes.Interface) error {
	klog.V(2).Info("PreStop: executing PreStop lifecycle hook")

	nodeName := os.Getenv("KUBE_NODE_NAME")
	if nodeName == "" {
		return errors.New("PreStop: KUBE_NODE_NAME missing")
	}

	node, err := fetchNode(clientset, nodeName)
	switch {
	case k8serrors.IsNotFound(err):
		klog.V(2).Infof("PreStop: node(%s) does not exist - assuming this is a termination event, checking for remaining VolumeAttachments", nodeName)
	case err != nil:
		return err
	case !isNodeBeingDrained(node):
		klog.V(2).Infof("PreStop: node(%s) is not being drained, skipping VolumeAttachments check node", nodeName)
		return nil
	default:
		klog.V(2).Infof("PreStop: node(%s) is being drained, checking for remaining VolumeAttachments", nodeName)
	}

	return waitForVolumeAttachments(clientset, nodeName)
}

func fetchNode(clientset kubernetes.Interface, nodeName string) (*v1.Node, error) {
	node, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("fetchNode: failed to retrieve node information: %w", err)
	}
	return node, nil
}

// isNodeBeingDrained returns true if node resource has a known drain/eviction taint.
func isNodeBeingDrained(node *v1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if _, isDrainTaint := drainTaints[taint.Key]; isDrainTaint {
			return true
		}
	}
	return false
}

func waitForVolumeAttachments(clientset kubernetes.Interface, nodeName string) error {
	allAttachmentsDeleted := make(chan struct{})

	// Use sync.Once so that concurrent informer events cannot double-close the channel.
	var once sync.Once
	signalDone := func() { once.Do(func() { close(allAttachmentsDeleted) }) }

	factory := informers.NewSharedInformerFactory(clientset, 0)
	informer := factory.Storage().V1().VolumeAttachments().Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			va, ok := obj.(*storagev1.VolumeAttachment)
			if !ok {
				klog.Errorf("UpdateFunc: error asserting object as type VolumeAttachment obj %s", va)
				return
			}
			klog.V(2).Infof("DeleteFunc: VolumeAttachment %s deleted for node %s", va.Name, va.Spec.NodeName)
			if va.Spec.NodeName == nodeName {
				if err := checkVolumeAttachments(clientset, nodeName, signalDone); err != nil {
					klog.Errorf("checkVolumeAttachments failed: %v", err)
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			va, ok := newObj.(*storagev1.VolumeAttachment)
			if !ok {
				klog.Errorf("UpdateFunc: error asserting object as type VolumeAttachment obj %s", va)
				return
			}
			klog.V(2).Infof("UpdateFunc: VolumeAttachment %s updated for node %s", va.Name, va.Spec.NodeName)
			if va.Spec.NodeName == nodeName {
				if err := checkVolumeAttachments(clientset, nodeName, signalDone); err != nil {
					klog.Errorf("checkVolumeAttachments failed: %v", err)
				}
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler to VolumeAttachment informer: %w", err)
	}

	go informer.Run(allAttachmentsDeleted)

	// Run an initial check now; subsequent checks are triggered by informer events.
	if err := checkVolumeAttachments(clientset, nodeName, signalDone); err != nil {
		klog.Errorf("checkVolumeAttachments failed: %v", err)
	}

	<-allAttachmentsDeleted
	klog.V(2).Info("waitForVolumeAttachments: finished waiting for VolumeAttachments to be deleted. preStopHook completed")
	return nil
}

// computeVAName returns the deterministic VolumeAttachment name assigned by the
// Kubernetes attach/detach controller:
//
//	"csi-" + hex(sha256(volumeHandle + attacher + nodeName))

func computeVAName(volumeHandle, attacher, nodeName string) string {
	sum := sha256.Sum256([]byte(volumeHandle + attacher + nodeName))
	return "csi-" + hex.EncodeToString(sum[:])
}

// localCSIPVNames returns the deduplicated PV names for CSI volumes currently
// present in the kubelet's local volume directories on this node. It scans both
// the filesystem-volume path and the raw-block-volume path:

func localCSIPVNames(globPatterns ...string) ([]string, error) {
	seen := make(map[string]struct{})
	var names []string
	for _, pattern := range globPatterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("localCSIPVNames: glob failed for %s: %w", pattern, err)
		}
		for _, m := range matches {
			name := filepath.Base(m)
			if _, dup := seen[name]; !dup {
				seen[name] = struct{}{}
				names = append(names, name)
			}
		}
	}
	return names, nil
}

func checkVolumeAttachments(clientset kubernetes.Interface, nodeName string, signalDone func()) error {
	pvNames, err := localCSIPVNames(kubeletCSIDir, kubeletCSIBlockDevicesDir)
	if err != nil {
		return fmt.Errorf("checkVolumeAttachments: failed to list local CSI PVs: %w", err)
	}
	klog.V(2).Infof("checkVolumeAttachments: %d local CSI PV(s) found, nodeName: %s", len(pvNames), nodeName)

	for _, pvName := range pvNames {
		pv, err := clientset.CoreV1().PersistentVolumes().Get(
			context.Background(), pvName, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			// PV already deleted from the API server; no VA can exist for it.
			continue
		}
		if err != nil {
			return fmt.Errorf("checkVolumeAttachments: failed to get PV %s: %w", pvName, err)
		}
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != azureDiskCSIDriver {
			continue
		}

		vaName := computeVAName(pv.Spec.CSI.VolumeHandle, azureDiskCSIDriver, nodeName)
		_, err = clientset.StorageV1().VolumeAttachments().Get(
			context.Background(), vaName, metav1.GetOptions{})
		if err == nil {
			// VA still exists — not ready to exit yet.
			klog.V(2).Infof("checkVolumeAttachments: VA %s still exists for PV %s, waiting", vaName, pvName)
			return nil
		}
		if !k8serrors.IsNotFound(err) {
			klog.Warningf("checkVolumeAttachments: error getting VA %s: %v", vaName, err)
			return fmt.Errorf("checkVolumeAttachments: error getting VA %s: %w", vaName, err)
		}
		// IsNotFound — this VA is already gone.
		klog.V(2).Infof("checkVolumeAttachments: VA %s for PV %s already removed", vaName, pvName)
	}

	// Every local PV's VA is gone (or was never an Azure disk VA).
	klog.V(2).Info("checkVolumeAttachments: no remaining VolumeAttachments found, signaling completion")
	signalDone()
	return nil
}
