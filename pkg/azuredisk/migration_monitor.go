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

package azuredisk

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

const (
	// Migration monitoring constants
	migrationCheckInterval     = 30 * time.Second
	migrationTimeout           = 24 * time.Hour
	progressReportingThreshold = 20 // Report every 20% completion

	// Event types for Kubernetes events
	EventTypeNormal  = "Normal"
	EventTypeWarning = "Warning"

	// Event reasons
	ReasonSKUMigrationStarted   = "SKUMigrationStarted"
	ReasonSKUMigrationProgress  = "SKUMigrationProgress"
	ReasonSKUMigrationCompleted = "SKUMigrationCompleted"
	ReasonSKUMigrationFailed    = "SKUMigrationFailed"
	ReasonSKUMigrationTimeout   = "SKUMigrationTimeout"

	// Operation name for monitoring
	MonitorOperationName = "DisksClient.MonitorMigration"
)

// MigrationTask represents an active disk migration monitoring task
type MigrationTask struct {
	DiskURI              string
	PVName               string
	FromSKU              string
	ToSKU                string
	StartTime            time.Time
	LastReportedProgress float32
	Context              context.Context
	CancelFunc           context.CancelFunc
}

// MigrationProgressMonitor monitors disk migration progress using Azure SDK polling
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
func (m *MigrationProgressMonitor) StartMigrationMonitoring(ctx context.Context, diskURI, pvName, fromSKU, toSKU string) error {
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
	go m.monitorMigrationWithPoller(context.TODO(), task)

	// Emit start event
	m.emitMigrationEvent(task, EventTypeNormal, ReasonSKUMigrationStarted,
		fmt.Sprintf("Started SKU migration from %s to %s for disk %s", fromSKU, toSKU, pvName))

	klog.V(2).Infof("Started migration monitoring for disk %s (%s -> %s)", pvName, fromSKU, toSKU)
	return nil
}

// monitorWithPollerWrapper uses utils.NewPollerWrapper with a custom poller
func (m *MigrationProgressMonitor) monitorMigrationWithPoller(ctx context.Context, task *MigrationTask) {
	// Create a runtime.Poller using NewPoller with custom handler
	poller, err := runtime.NewPoller(nil, nil, &runtime.NewPollerOptions[armcompute.DisksClientGetResponse]{
		Handler: NewMigrationPoller(task, m, ctx),
	})

	if err != nil {
		klog.Errorf("Failed to create poller for disk %s: %v", task.DiskURI, err)
		m.emitMigrationEvent(task, EventTypeWarning, ReasonSKUMigrationFailed,
			fmt.Sprintf("Failed to create poller: %v", err))
		return
	}

	// Create wrapper and wait for response
	pollerWrapper := utils.NewPollerWrapper[armcompute.DisksClientGetResponse](poller, nil)
	resp, err := pollerWrapper.WaitforPollerResp(ctx)
	if err != nil {
		klog.Errorf("Migration monitoring failed for disk %s: %v", task.DiskURI, err)
		m.emitMigrationEvent(task, EventTypeWarning, ReasonSKUMigrationFailed,
			fmt.Sprintf("Migration monitoring failed: %v", err))
		return
	}

	// Check the response
	if resp.Disk.SKU != nil {
		m.emitMigrationEvent(task, EventTypeNormal, ReasonSKUMigrationCompleted,
			fmt.Sprintf("Successfully completed SKU migration from %s to %s for volume %s (duration: %v)",
				task.FromSKU, task.ToSKU, task.PVName, time.Since(task.StartTime)))
		klog.V(2).Infof("Migration completed for disk %s", task.DiskURI)
	}
}

// MigrationPoller implements runtime.Poller[armcompute.DisksClientUpdateResponse] interface for migration monitoring
type MigrationPoller struct {
	runtime.Poller[armcompute.DisksClientUpdateResponse]
	task    *MigrationTask
	monitor *MigrationProgressMonitor
	ctx     context.Context
	done    bool
}

// NewMigrationPoller returns a MigrationPoller that embeds a dummy runtime.Poller
func NewMigrationPoller(task *MigrationTask, monitor *MigrationProgressMonitor, ctx context.Context) *MigrationPoller {
	// Create a dummy poller to satisfy the interface
	var dummyPoller runtime.Poller[armcompute.DisksClientUpdateResponse]
	return &MigrationPoller{
		Poller:  dummyPoller,
		task:    task,
		monitor: monitor,
		ctx:     ctx,
		done:    false,
	}
}

// PollUntilDone implements runtime.Poller[armcompute.DisksClientUpdateResponse] interface using checkMigrationProgressWithPoller
func (p *MigrationPoller) PollUntilDone(ctx context.Context, options *runtime.PollUntilDoneOptions) (armcompute.DisksClientUpdateResponse, error) {
	// Use the frequency from options or default to 5 seconds like Azure SDK
	frequency := 5 * time.Second
	if options != nil && options.Frequency > 0 {
		frequency = options.Frequency
	}

	ticker := time.NewTicker(frequency)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return armcompute.DisksClientUpdateResponse{}, ctx.Err()

		case <-ticker.C:
			// Call our existing checkMigrationProgressWithPoller method
			completed, err := p.monitor.checkMigrationProgressWithPoller(p.task)
			if err != nil {
				klog.V(4).Infof("Progress check error (will retry): %v", err)
				continue // Continue on errors, same as Azure SDK pattern
			}

			if completed {
				// Mark as done and return the response that utils.NewPollerWrapper expects
				p.done = true

				// Create a proper response that matches armcompute.DisksClientUpdateResponse
				response := armcompute.DisksClientUpdateResponse{
					Disk: armcompute.Disk{
						Name: &p.task.PVName,
						SKU: &armcompute.DiskSKU{
							Name: (*armcompute.DiskStorageAccountTypes)(&p.task.ToSKU),
						},
						Properties: &armcompute.DiskProperties{
							CompletionPercent: &[]float32{100}[0], // 100% complete
						},
					},
				}
				return response, nil
			}
		}
	}
}

// Poll implements runtime.Poller interface (required but we use PollUntilDone)
func (p *MigrationPoller) Poll(ctx context.Context) (*http.Response, error) {
	// Call checkMigrationProgressWithPoller and return a mock HTTP response
	completed, err := p.monitor.checkMigrationProgressWithPoller(p.task)
	if err != nil {
		return nil, err
	}

	if completed {
		p.done = true
		// Return a successful HTTP response
		return &http.Response{
			StatusCode: 200,
			Status:     "200 OK",
		}, nil
	}

	// Return a 202 Accepted response to indicate polling continues
	return &http.Response{
		StatusCode: 202,
		Status:     "202 Accepted",
	}, nil

}

// Result implements runtime.Poller interface
func (p *MigrationPoller) Result(ctx context.Context) (armcompute.DisksClientUpdateResponse, error) {
	if !p.done {
		return armcompute.DisksClientUpdateResponse{}, fmt.Errorf("migration not yet complete")
	}

	// Return the final result
	response := armcompute.DisksClientUpdateResponse{
		Disk: armcompute.Disk{
			Name: &p.task.PVName,
			SKU: &armcompute.DiskSKU{
				Name: (*armcompute.DiskStorageAccountTypes)(&p.task.ToSKU),
			},
			Properties: &armcompute.DiskProperties{
				CompletionPercent: &[]float32{100}[0],
			},
		},
	}
	return response, nil
}

// Done implements runtime.Poller interface
func (p *MigrationPoller) Done() bool {
	return p.done
}

// checkMigrationProgressWithPoller checks migration progress using Azure SDK poller
func (m *MigrationProgressMonitor) checkMigrationProgressWithPoller(task *MigrationTask) (bool, error) {
	// Get disk client from disk controller
	disk, error := m.diskController.GetDiskByURI(task.Context, task.DiskURI)
	if error != nil {
		return false, fmt.Errorf("failed to get disk %s: %v", task.DiskURI, error)
	}

	// Check if migration is complete by comparing SKU
	if disk.SKU != nil && string(*disk.SKU.Name) == task.ToSKU {
		return true, nil
	}

	// Check completion percentage if available
	var completionPercent float32
	if disk.Properties != nil && disk.Properties.CompletionPercent != nil {
		completionPercent = *disk.Properties.CompletionPercent
	}

	// Report progress if significant milestone reached
	if m.shouldReportProgress(completionPercent, task.LastReportedProgress) {
		m.emitMigrationEvent(task, EventTypeNormal, ReasonSKUMigrationProgress,
			fmt.Sprintf("Migration progress: %f%% complete for volume %s (elapsed: %v)",
				completionPercent, task.PVName, time.Since(task.StartTime)))
		task.LastReportedProgress = completionPercent
		klog.V(2).Infof("Migration progress for disk %s: %f%% complete", task.DiskURI, completionPercent)
	}

	return false, nil
}

// shouldReportProgress determines if progress should be reported based on milestones
func (m *MigrationProgressMonitor) shouldReportProgress(current, last float32) bool {
	// Report at every 20% milestone and at 100%
	currentMilestone := (current / progressReportingThreshold) * progressReportingThreshold
	lastMilestone := (last / progressReportingThreshold) * progressReportingThreshold

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

	// Emit event
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
