/*
Copyright 2025 The Kubernetes Authors.

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

package azuredisk

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

const (
	// Migration monitoring constants
	migrationCheckInterval     = 30 * time.Second
	migrationTimeout           = 24 * time.Hour
	progressReportingThreshold = 20 // Report every 20% completion

	// Event types and reasons
	EventTypeNormal  = "Normal"
	EventTypeWarning = "Warning"

	ReasonSKUMigrationStarted   = "SKUMigrationStarted"
	ReasonSKUMigrationProgress  = "SKUMigrationProgress"
	ReasonSKUMigrationCompleted = "SKUMigrationCompleted"
	ReasonSKUMigrationFailed    = "SKUMigrationFailed"
	ReasonSKUMigrationTimeout   = "SKUMigrationTimeout"
)

// MigrationTask represents an active disk migration monitoring task
type MigrationTask struct {
	DiskURI              string
	PVName               string
	FromSKU              armcompute.DiskStorageAccountTypes
	ToSKU                armcompute.DiskStorageAccountTypes
	StartTime            time.Time
	LastReportedProgress float32
	Context              context.Context
	CancelFunc           context.CancelFunc
}

// MigrationProgressMonitor monitors disk migration progress
type MigrationProgressMonitor struct {
	kubeClient     kubernetes.Interface
	eventRecorder  record.EventRecorder
	diskController *ManagedDiskController
	activeTasks    map[string]*MigrationTask
	mutex          sync.RWMutex
}

// NewMigrationProgressMonitor creates a new migration progress monitor
func NewMigrationProgressMonitor(kubeClient kubernetes.Interface, eventRecorder record.EventRecorder, diskController *ManagedDiskController) *MigrationProgressMonitor {
	return &MigrationProgressMonitor{
		kubeClient:     kubeClient,
		eventRecorder:  eventRecorder,
		diskController: diskController,
		activeTasks:    make(map[string]*MigrationTask),
	}
}

// StartMigrationMonitoring starts monitoring a disk migration with progress updates
func (m *MigrationProgressMonitor) StartMigrationMonitoring(ctx context.Context, diskURI, pvName string, fromSKU, toSKU armcompute.DiskStorageAccountTypes) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Check if already monitoring this disk
	if _, exists := m.activeTasks[diskURI]; exists {
		klog.V(2).Infof("Migration monitoring already active for disk %s", diskURI)
		return nil
	}

	// Create task context with timeout
	taskCtx, cancelFunc := context.WithTimeout(context.Background(), migrationTimeout)

	task := &MigrationTask{
		DiskURI:              diskURI,
		PVName:               pvName,
		FromSKU:              fromSKU,
		ToSKU:                toSKU,
		StartTime:            time.Now(),
		LastReportedProgress: 0,
		Context:              taskCtx,
		CancelFunc:           cancelFunc,
	}

	m.activeTasks[diskURI] = task

	// Start async monitoring
	go m.monitorMigrationProgress(task)

	// Emit start event
	m.emitMigrationEvent(task, EventTypeNormal, ReasonSKUMigrationStarted,
		fmt.Sprintf("Started SKU migration from %s to %s for volume %s", fromSKU, toSKU, pvName))

	klog.V(2).Infof("Started migration monitoring for disk %s (%s -> %s)", pvName, fromSKU, toSKU)
	return nil
}

// monitorMigrationProgress monitors the progress of a disk migration
func (m *MigrationProgressMonitor) monitorMigrationProgress(task *MigrationTask) {
	defer func() {
		m.mutex.Lock()
		delete(m.activeTasks, task.DiskURI)
		m.mutex.Unlock()
		task.CancelFunc()
	}()

	ticker := time.NewTicker(migrationCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-task.Context.Done():
			if task.Context.Err() == context.DeadlineExceeded {
				m.emitMigrationEvent(task, EventTypeWarning, ReasonSKUMigrationTimeout,
					fmt.Sprintf("Migration timeout after %v for volume %s", migrationTimeout, task.PVName))
				klog.Warningf("Migration timeout for disk %s after %v", task.DiskURI, migrationTimeout)
			}
			return

		case <-ticker.C:
			completed, err := m.checkMigrationProgress(task)
			if err != nil {
				klog.V(4).Infof("Progress check error for disk %s (will retry): %v", task.DiskURI, err)
				continue
			}

			if completed {
				m.emitMigrationEvent(task, EventTypeNormal, ReasonSKUMigrationCompleted,
					fmt.Sprintf("Successfully completed SKU migration from %s to %s for volume %s (duration: %v)",
						task.FromSKU, task.ToSKU, task.PVName, time.Since(task.StartTime)))
				klog.V(2).Infof("Migration completed for disk %s in %v", task.DiskURI, time.Since(task.StartTime))
				return
			}
		}
	}
}

// checkMigrationProgress checks the current progress of a disk migration
func (m *MigrationProgressMonitor) checkMigrationProgress(task *MigrationTask) (bool, error) {
	// Get current disk state
	disk, err := m.diskController.GetDiskByURI(task.Context, task.DiskURI)
	if err != nil {
		return false, fmt.Errorf("failed to get disk %s: %v", task.DiskURI, err)
	}

	// Check completion percentage if available
	var completionPercent float32
	if disk.Properties != nil && disk.Properties.CompletionPercent != nil {
		completionPercent = *disk.Properties.CompletionPercent
	}

	// Report progress if significant milestone reached
	if m.shouldReportProgress(completionPercent, task.LastReportedProgress) {
		m.emitMigrationEvent(task, EventTypeNormal, ReasonSKUMigrationProgress,
			fmt.Sprintf("Migration progress: %.1f%% complete for volume %s (elapsed: %v)",
				completionPercent, task.PVName, time.Since(task.StartTime)))
		task.LastReportedProgress = completionPercent
		klog.V(2).Infof("Migration progress for disk %s: %.1f%% complete", task.DiskURI, completionPercent)
	}

	return false, nil
}

// shouldReportProgress determines if progress should be reported based on milestones
func (m *MigrationProgressMonitor) shouldReportProgress(current, last float32) bool {
	// Report at every 20% milestone
	currentMilestone := int(current/progressReportingThreshold) * progressReportingThreshold
	lastMilestone := int(last/progressReportingThreshold) * progressReportingThreshold

	return current == 100 || (currentMilestone > lastMilestone && currentMilestone > 0)
}

// emitMigrationEvent emits a Kubernetes event for the PersistentVolume
func (m *MigrationProgressMonitor) emitMigrationEvent(task *MigrationTask, eventType, reason, message string) {
	if m.eventRecorder == nil || m.kubeClient == nil {
		klog.V(4).Infof("Event recorder or kube client not available, skipping event: %s", message)
		return
	}

	// Get PersistentVolume object
	pv, err := m.kubeClient.CoreV1().PersistentVolumes().Get(context.TODO(), task.PVName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get PersistentVolume %s for event emission: %v", task.PVName, err)
		return
	}

	// Emit event on the PersistentVolume
	m.eventRecorder.Event(pv, eventType, reason, message)
	klog.V(4).Infof("Emitted event for PV %s: %s - %s", task.PVName, reason, message)
}

// GetActiveMigrations returns currently active migration tasks
func (m *MigrationProgressMonitor) GetActiveMigrations() map[string]*MigrationTask {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	result := make(map[string]*MigrationTask)
	for k, v := range m.activeTasks {
		taskCopy := *v
		result[k] = &taskCopy
	}
	return result
}

// IsMigrationActive checks if migration is currently being monitored for a disk
func (m *MigrationProgressMonitor) IsMigrationActive(diskURI string) bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	_, exists := m.activeTasks[diskURI]
	return exists
}

// Stop stops all active migration monitoring tasks
func (m *MigrationProgressMonitor) Stop() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for _, task := range m.activeTasks {
		if task.CancelFunc != nil {
			task.CancelFunc()
		}
	}

	klog.V(2).Infof("Migration progress monitor stopped, cancelled %d active tasks", len(m.activeTasks))
}
