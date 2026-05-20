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
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils"
)

const (
	// Migration monitoring constants
	progressReportingThreshold = 20 // Report every 20% completion

	ReasonSKUMigrationStarted   = "SKUMigrationStarted"
	ReasonSKUMigrationProgress  = "SKUMigrationProgress"
	ReasonSKUMigrationCompleted = "SKUMigrationCompleted"
	ReasonSKUMigrationTimeout   = "SKUMigrationTimeout"

	// Label for migration status on PVC
	LabelMigrationInProgress = "disk.csi.azure.com/migration-in-progress"

	// Volume size thresholds (in bytes)
	volumeSize2TB  = 2 * 1024 * 1024 * 1024 * 1024  // 2TB
	volumeSize4TB  = 4 * 1024 * 1024 * 1024 * 1024  // 4TB
	volumeSize16TB = 16 * 1024 * 1024 * 1024 * 1024 // 16TB
	volumeSize64TB = 64 * 1024 * 1024 * 1024 * 1024 // 64TB

	// Timeout durations based on volume size
	migrationTimeoutBelowTwoTB       = 10 * time.Hour // Below 2TB
	migrationTimeoutBelowFourTB      = 12 * time.Hour // 2TB to 4TB
	migrationTimeoutBelowSixteenTB   = 16 * time.Hour // 4TB to 16TB
	migrationTimeoutBelowSixtyFourTB = 19 * time.Hour // 16TB to 64TB

	// Worker pool defaults
	defaultMaxWorkers = 50

	// Adaptive polling intervals based on migration progress (defaults)
	defaultPollIntervalSlowPhase   = 5 * time.Minute  // 0-20% progress: migration just started, changes are slow
	defaultPollIntervalNormalPhase = 60 * time.Second // 20-80% progress: active migration
	defaultPollIntervalFastPhase   = 30 * time.Second // 80-99% progress: nearly done, check more often

	// Backoff: if no progress change after consecutive polls, increase interval
	maxConsecutiveNoChange = 3
	backoffMultiplier      = 2
	maxBackoffInterval     = 10 * time.Minute
)

var (
	migrationCheckInterval = 30 * time.Second // base tick interval for the polling loop
	// Migration timeout map
	migrationTimeouts = map[int64]time.Duration{
		volumeSize2TB:  migrationTimeoutBelowTwoTB,
		volumeSize4TB:  migrationTimeoutBelowFourTB,
		volumeSize16TB: migrationTimeoutBelowSixteenTB,
		volumeSize64TB: migrationTimeoutBelowSixtyFourTB,
	}

	// timeout array for small, medium and large volumes
	sortedMigrationSlabArray = []int64{
		volumeSize2TB,  // 2TB
		volumeSize4TB,  // 4TB
		volumeSize16TB, // 16TB
		volumeSize64TB, // 64TB
	}

	// Maximum migration timeout
	maxMigrationTimeout = 24 * time.Hour // Maximum allowed timeout for any migration

	// Maximum concurrent ARM API calls for migration progress checks
	maxMigrationWorkers = defaultMaxWorkers
)

// getMigrationTimeout returns the appropriate timeout based on volume size
func getMigrationTimeout(volumeSize int64) time.Duration {
	for _, slab := range sortedMigrationSlabArray {
		if volumeSize < slab {
			if timeout, exists := migrationTimeouts[slab]; exists {
				return timeout
			}
			break
		}
	}
	return migrationTimeoutBelowSixteenTB
}

func initializeTimeouts() {
	if migrationTimeoutsEnv := os.Getenv("MIGRATION_TIMEOUTS"); migrationTimeoutsEnv != "" {
		for _, pair := range strings.Split(migrationTimeoutsEnv, ",") {
			parts := strings.Split(pair, "=")
			if len(parts) != 2 {
				klog.Warningf("Invalid migration timeout format: %s", pair)
				continue
			}

			sizeStr := parts[0]
			timeoutStr := parts[1]

			var size resource.Quantity
			var timeout time.Duration
			var err error

			if size, err = resource.ParseQuantity(sizeStr); err != nil {
				klog.Warningf("Invalid migration timeout size: %s", sizeStr)
				continue
			}

			if timeout, err = time.ParseDuration(timeoutStr); err != nil {
				klog.Warningf("Invalid migration timeout duration: %s", timeoutStr)
				continue
			}

			migrationTimeouts[size.Value()] = timeout
			sortedMigrationSlabArray = append(sortedMigrationSlabArray, size.Value())
		}
		sort.Slice(sortedMigrationSlabArray, func(i, j int) bool {
			return sortedMigrationSlabArray[i] < sortedMigrationSlabArray[j]
		})

		klog.V(4).Infof("Sorted migration slab array: %v", sortedMigrationSlabArray)
	}

	if maxMigrationTimeoutEnv := os.Getenv("MAX_MIGRATION_TIMEOUT"); maxMigrationTimeoutEnv != "" {
		if duration, err := time.ParseDuration(maxMigrationTimeoutEnv); err == nil {
			maxMigrationTimeout = duration
		}
	}

	if maxWorkersEnv := os.Getenv("MAX_MIGRATION_WORKERS"); maxWorkersEnv != "" {
		if workers, err := strconv.Atoi(maxWorkersEnv); err == nil && workers > 0 {
			maxMigrationWorkers = workers
		}
	}
}

func init() {
	initializeTimeouts()
}

// MigrationTask represents an active disk migration monitoring task
type MigrationTask struct {
	DiskURI              string
	PVName               string
	PVCName              string
	PVCNamespace         string
	FromSKU              string
	ToSKU                armcompute.DiskStorageAccountTypes
	StartTime            time.Time
	LastReportedProgress float32
	VolumeSize           int64         // Volume size in bytes
	MigrationTimeout     time.Duration // Calculated timeout based on volume size
	Timedout             bool          // Indicates if the task has timed out
	Cancelled            atomic.Bool   // Indicates if the task was cancelled
	PVLabeled            bool          // Indicates if the PV has been labelled for migration

	// Adaptive polling fields
	LastPollTime        time.Time     // When this task was last polled
	ConsecutiveNoChange int           // Count of polls with no progress change
	CurrentPollInterval time.Duration // Dynamically adjusted poll interval
	TimeoutEventEmitted bool          // Whether the initial timeout warning was emitted

	mutex sync.RWMutex // Ensures disk level migration details are accessed safely
}

// MigrationProgressMonitor monitors disk migration progress using a centralized polling loop
// with a bounded worker pool instead of per-task goroutines.
type MigrationProgressMonitor struct {
	kubeClient     kubernetes.Interface
	eventRecorder  record.EventRecorder
	diskController *ManagedDiskController
	activeTasks    map[string]*MigrationTask
	mutex          sync.RWMutex // Ensures the activeTasks map is accessed safely

	// Worker pool: semaphore to limit concurrent ARM API calls
	workerSem chan struct{}

	// Adaptive polling intervals (per-instance for testability)
	pollIntervalSlowPhase   time.Duration
	pollIntervalNormalPhase time.Duration
	pollIntervalFastPhase   time.Duration
	checkInterval           time.Duration // base tick interval for the polling loop

	// Lifecycle management
	ctx        context.Context
	cancelFunc context.CancelFunc
	stopped    atomic.Bool
}

// NewMigrationProgressMonitor creates a new migration progress monitor with a centralized polling loop
func NewMigrationProgressMonitor(kubeClient kubernetes.Interface, eventRecorder record.EventRecorder, diskController *ManagedDiskController) *MigrationProgressMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	m := &MigrationProgressMonitor{
		kubeClient:              kubeClient,
		eventRecorder:           eventRecorder,
		diskController:          diskController,
		activeTasks:             make(map[string]*MigrationTask),
		workerSem:               make(chan struct{}, maxMigrationWorkers),
		pollIntervalSlowPhase:   defaultPollIntervalSlowPhase,
		pollIntervalNormalPhase: defaultPollIntervalNormalPhase,
		pollIntervalFastPhase:   defaultPollIntervalFastPhase,
		checkInterval:           migrationCheckInterval,
		ctx:                     ctx,
		cancelFunc:              cancel,
	}

	// Start the centralized polling loop
	go m.runPollingLoop()

	return m
}

// StartMigrationMonitoring registers a disk migration for monitoring by the centralized polling loop
func (m *MigrationProgressMonitor) StartMigrationMonitoring(ctx context.Context, isProvisioningFlow bool, diskURI, pvName string, fromSKU string, toSKU armcompute.DiskStorageAccountTypes, volumeSize int64) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if _, exists := m.activeTasks[diskURI]; exists {
		klog.V(2).Infof("Migration monitoring already active for disk %s", diskURI)
		return nil
	}

	var pvcName, pvcNamespace string

	if !isProvisioningFlow && pvName != "" {
		pv, err := m.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get PV %s: %v", pvName, err)
		}

		if pv.Spec.ClaimRef == nil {
			klog.V(2).Infof("PV %s has no claim reference, skipping migration monitoring", pvName)
			return nil
		}

		pvcName = pv.Spec.ClaimRef.Name
		pvcNamespace = pv.Spec.ClaimRef.Namespace
	}

	migrationTimeout := getMigrationTimeout(volumeSize)

	task := &MigrationTask{
		DiskURI:              diskURI,
		PVName:               pvName,
		PVCName:              pvcName,
		PVCNamespace:         pvcNamespace,
		FromSKU:              fromSKU,
		ToSKU:                toSKU,
		StartTime:            time.Now(),
		LastReportedProgress: 0,
		VolumeSize:           volumeSize,
		MigrationTimeout:     migrationTimeout,
		PVLabeled:            false,
		CurrentPollInterval:  m.pollIntervalNormalPhase,
	}

	m.activeTasks[diskURI] = task

	// Add label to PV to track migration state
	labelExisted, err := m.addMigrationLabelIfNotExists(ctx, pvName, fromSKU, toSKU)
	if err != nil {
		klog.Warningf("Failed to add migration label to PV %s: %v", pvName, err)
	} else {
		task.PVLabeled = true
	}

	klog.V(2).Infof("Using migration timeout of %v for volume %s (max workers: %d)", migrationTimeout, pvName, maxMigrationWorkers)

	if !isProvisioningFlow && !labelExisted {
		_ = m.emitMigrationEvent(task, corev1.EventTypeNormal, ReasonSKUMigrationStarted,
			fmt.Sprintf("Started monitoring SKU migration from %s to %s for volume %s (timeout: %v)", fromSKU, toSKU, pvName, migrationTimeout))
		klog.V(2).Infof("Started migration monitoring for disk %s (%s -> %s)", pvName, fromSKU, toSKU)
	} else {
		klog.V(2).Infof("Resumed migration monitoring for disk %s (%s -> %s)", pvName, fromSKU, toSKU)
	}

	return nil
}

// runPollingLoop is the single centralized loop that periodically dispatches progress checks
func (m *MigrationProgressMonitor) runPollingLoop() {
	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.pollReadyTasks()
		}
	}
}

// pollReadyTasks dispatches progress checks for tasks that are due for polling
func (m *MigrationProgressMonitor) pollReadyTasks() {
	m.mutex.RLock()
	tasks := make([]*MigrationTask, 0, len(m.activeTasks))
	for _, task := range m.activeTasks {
		if task.Cancelled.Load() {
			continue
		}
		if m.shouldPollTask(task) {
			tasks = append(tasks, task)
		}
	}
	m.mutex.RUnlock()

	if len(tasks) == 0 {
		return
	}

	klog.V(4).Infof("Polling %d migration tasks (of %d active)", len(tasks), m.getActiveCount())

	var wg sync.WaitGroup
	for i, task := range tasks {
		// Add jitter to spread requests across time and avoid thundering herd
		if i > 0 && len(tasks) > 1 {
			jitter := time.Duration(rand.Int63n(int64(500 * time.Millisecond)))
			time.Sleep(jitter)
		}

		// Acquire worker slot (blocks if all workers are busy)
		select {
		case m.workerSem <- struct{}{}:
		case <-m.ctx.Done():
			return
		}

		wg.Add(1)
		go func(t *MigrationTask) {
			defer wg.Done()
			defer func() { <-m.workerSem }()
			m.processTask(t)
		}(task)
	}
	wg.Wait()
}

// shouldPollTask determines if a task is due for its next poll based on adaptive intervals
func (m *MigrationProgressMonitor) shouldPollTask(task *MigrationTask) bool {
	task.mutex.RLock()
	defer task.mutex.RUnlock()

	if task.LastPollTime.IsZero() {
		return true
	}

	// Use the smaller of adaptive interval and migrationCheckInterval
	// This ensures tests with short intervals still work correctly
	return time.Since(task.LastPollTime) >= task.CurrentPollInterval
}

// getAdaptiveInterval returns the polling interval based on progress and backoff state
func (m *MigrationProgressMonitor) getAdaptiveInterval(progress float32, consecutiveNoChange int) time.Duration {
	var baseInterval time.Duration
	switch {
	case progress < 20:
		baseInterval = m.pollIntervalSlowPhase
	case progress >= 80:
		baseInterval = m.pollIntervalFastPhase
	default:
		baseInterval = m.pollIntervalNormalPhase
	}

	// Apply backoff if progress is stalled
	if consecutiveNoChange >= maxConsecutiveNoChange {
		backoff := baseInterval * time.Duration(backoffMultiplier)
		if backoff > maxBackoffInterval {
			backoff = maxBackoffInterval
		}
		return backoff
	}

	return baseInterval
}

// processTask handles a single task: timeout checks, progress check, and completion
func (m *MigrationProgressMonitor) processTask(task *MigrationTask) {
	task.mutex.Lock()
	task.LastPollTime = time.Now()
	task.mutex.Unlock()

	if task.Cancelled.Load() {
		return
	}

	// Check if task exceeded maximum migration timeout
	elapsed := time.Since(task.StartTime)
	if elapsed >= maxMigrationTimeout {
		_ = m.emitMigrationEvent(task, corev1.EventTypeWarning, ReasonSKUMigrationTimeout,
			fmt.Sprintf("Stopping monitoring the migration for the disk %s after waiting for %vh", task.DiskURI, maxMigrationTimeout.Hours()))
		klog.Warningf("Migration monitoring for disk %s cancelling after waiting for %vh", task.DiskURI, maxMigrationTimeout.Hours())
		m.removeTask(task.DiskURI)
		return
	}

	// Emit initial timeout warning when per-size MigrationTimeout is exceeded
	if elapsed >= task.MigrationTimeout && !task.TimeoutEventEmitted {
		_ = m.emitMigrationEvent(task, corev1.EventTypeWarning, ReasonSKUMigrationTimeout,
			fmt.Sprintf("Migration taking too long (running %v hours) for volume %s", task.MigrationTimeout.Hours(), task.PVName))
		klog.Warningf("Migration taking too long (running %v hours) for disk %s", task.MigrationTimeout.Hours(), task.DiskURI)
		task.mutex.Lock()
		task.Timedout = true
		task.TimeoutEventEmitted = true
		task.mutex.Unlock()
	}

	// Try to label PV if not yet labeled (provisioning flow where PV may not exist yet)
	if !task.PVLabeled {
		_, err := m.addMigrationLabelIfNotExists(m.ctx, task.PVName, task.FromSKU, task.ToSKU)
		if err != nil {
			klog.Warningf("Failed to add migration label to PV %s: %v", task.PVName, err)
		} else {
			task.mutex.Lock()
			task.PVLabeled = true
			task.mutex.Unlock()
		}
	}

	// Resolve PVC info if missing (provisioning flow)
	task.mutex.RLock()
	pvcMissing := task.PVLabeled && (task.PVCName == "" || task.PVCNamespace == "")
	task.mutex.RUnlock()

	if pvcMissing {
		pv, err := m.kubeClient.CoreV1().PersistentVolumes().Get(m.ctx, task.PVName, metav1.GetOptions{})
		if err == nil && pv.Spec.ClaimRef != nil {
			task.mutex.Lock()
			task.PVCName = pv.Spec.ClaimRef.Name
			task.PVCNamespace = pv.Spec.ClaimRef.Namespace
			task.mutex.Unlock()
			_ = m.emitMigrationEvent(task, corev1.EventTypeNormal, ReasonSKUMigrationStarted,
				fmt.Sprintf("Started monitoring SKU migration from %s to %s for volume %s (timeout: %v)",
					task.FromSKU, task.ToSKU, task.PVName, task.MigrationTimeout))
			klog.V(2).Infof("Started migration monitoring for disk %s (%s -> %s)", task.PVName, task.FromSKU, task.ToSKU)
		}
	}

	// Check migration progress via ARM API (the only expensive call per task per poll)
	completed, err := m.checkMigrationProgress(task)
	if err != nil {
		klog.Warningf("Progress check error for disk %s (will retry): %v", task.DiskURI, err)
		return
	}

	if completed {
		if err := m.emitMigrationEvent(task, corev1.EventTypeNormal, ReasonSKUMigrationCompleted,
			fmt.Sprintf("Successfully completed SKU migration from %s to %s for volume %s (duration: %v)",
				task.FromSKU, task.ToSKU, task.PVName, time.Since(task.StartTime))); err != nil {
			klog.Errorf("Failed to emit completion event for disk %s: %v", task.DiskURI, err)
		}
		klog.V(2).Infof("Migration completed for disk %s in %v", task.DiskURI, time.Since(task.StartTime))

		if err := m.removeMigrationLabel(m.ctx, task.PVName); err != nil {
			klog.Warningf("Failed to remove migration label from PV %s: %v", task.PVName, err)
		}
		m.removeTask(task.DiskURI)
	}
}

// removeTask removes a completed or timed-out task from the active tasks map
func (m *MigrationProgressMonitor) removeTask(diskURI string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	delete(m.activeTasks, diskURI)
}

// checkMigrationProgress checks the current progress of a disk migration
func (m *MigrationProgressMonitor) checkMigrationProgress(task *MigrationTask) (bool, error) {
	disk, err := m.diskController.GetDiskByURI(m.ctx, task.DiskURI)
	if err != nil {
		return false, fmt.Errorf("failed to get disk %s: %v", task.DiskURI, err)
	}

	var completionPercent float32
	if disk.Properties != nil && disk.Properties.CompletionPercent != nil {
		completionPercent = *disk.Properties.CompletionPercent
	}

	// Update adaptive polling interval based on progress
	task.mutex.Lock()
	previousProgress := task.LastReportedProgress
	if completionPercent == previousProgress {
		task.ConsecutiveNoChange++
	} else {
		task.ConsecutiveNoChange = 0
	}
	task.CurrentPollInterval = m.getAdaptiveInterval(completionPercent, task.ConsecutiveNoChange)
	task.LastReportedProgress = completionPercent
	task.mutex.Unlock()

	// Report progress if significant milestone reached
	if m.shouldReportProgress(completionPercent, previousProgress) {
		_ = m.emitMigrationEvent(task, corev1.EventTypeNormal, ReasonSKUMigrationProgress,
			fmt.Sprintf("Migration progress: %.1f%% complete for volume %s (elapsed: %v)",
				completionPercent, task.PVName, time.Since(task.StartTime)))
		klog.V(2).Infof("Migration progress for disk %s: %.1f%% complete", task.DiskURI, completionPercent)
	}

	return completionPercent >= 100, nil
}

// shouldReportProgress determines if progress should be reported based on milestones
func (m *MigrationProgressMonitor) shouldReportProgress(current, last float32) bool {
	currentMilestone := int(current/progressReportingThreshold) * progressReportingThreshold
	lastMilestone := int(last/progressReportingThreshold) * progressReportingThreshold

	return current < 100 && (currentMilestone > lastMilestone && currentMilestone > 0)
}

// emitMigrationEvent emits a Kubernetes event for the PersistentVolumeClaim
func (m *MigrationProgressMonitor) emitMigrationEvent(task *MigrationTask, eventType, reason, message string) error {
	if m.eventRecorder == nil || m.kubeClient == nil {
		klog.Warningf("Event recorder or kube client not available, skipping event: %s", message)
		return fmt.Errorf("event recorder or kube client not available")
	}

	task.mutex.RLock()
	pvcName := task.PVCName
	pvcNamespace := task.PVCNamespace
	task.mutex.RUnlock()

	if pvcName == "" || pvcNamespace == "" {
		return nil
	}

	pvc, err := m.kubeClient.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(m.ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get PersistentVolumeClaim %s/%s for event emission: %v", pvcNamespace, pvcName, err)
		if apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	m.eventRecorder.Event(pvc, eventType, reason, message)
	klog.V(4).Infof("Emitted event for PVC %s/%s: %s - %s", pvcNamespace, pvcName, reason, message)
	return nil
}

// addMigrationLabelIfNotExists adds migration label to the PersistentVolume if it doesn't already exist
// Returns true if label already existed, false if it was newly added
func (m *MigrationProgressMonitor) addMigrationLabelIfNotExists(ctx context.Context, pvName string, fromSKU string, toSKU armcompute.DiskStorageAccountTypes) (bool, error) {
	pv, err := m.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}

	if pv.Labels != nil {
		if value, exists := pv.Labels[LabelMigrationInProgress]; exists && value == "true" {
			klog.V(2).Infof("Migration label already exists for PV %s (%s -> %s)", pvName, fromSKU, toSKU)
			return true, nil
		}
	}

	if pv.Labels == nil {
		pv.Labels = make(map[string]string)
	}

	pv.Labels[LabelMigrationInProgress] = "true"

	_, err = m.kubeClient.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
	return false, err
}

// removeMigrationLabel removes migration label from the PersistentVolume
func (m *MigrationProgressMonitor) removeMigrationLabel(ctx context.Context, pvName string) error {
	pv, err := m.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	delete(pv.Labels, LabelMigrationInProgress)

	_, err = m.kubeClient.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
	return err
}

// GetActiveMigrations returns currently active migration tasks
func (m *MigrationProgressMonitor) GetActiveMigrations() map[string]*MigrationTask {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	result := make(map[string]*MigrationTask)
	for k, v := range m.activeTasks {
		v.mutex.RLock()
		taskCopy := &MigrationTask{
			DiskURI:              v.DiskURI,
			PVName:               v.PVName,
			PVCName:              v.PVCName,
			PVCNamespace:         v.PVCNamespace,
			FromSKU:              v.FromSKU,
			ToSKU:                v.ToSKU,
			StartTime:            v.StartTime,
			LastReportedProgress: v.LastReportedProgress,
			VolumeSize:           v.VolumeSize,
			MigrationTimeout:     v.MigrationTimeout,
			Timedout:             v.Timedout,
			LastPollTime:         v.LastPollTime,
			ConsecutiveNoChange:  v.ConsecutiveNoChange,
			CurrentPollInterval:  v.CurrentPollInterval,
		}
		taskCopy.Cancelled.Store(v.Cancelled.Load())
		v.mutex.RUnlock()
		result[k] = taskCopy
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

// getActiveCount returns the number of active migration tasks
func (m *MigrationProgressMonitor) getActiveCount() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return len(m.activeTasks)
}

// Stop stops all active migration monitoring tasks and the polling loop
func (m *MigrationProgressMonitor) Stop() {
	if m.stopped.Load() {
		return
	}
	m.stopped.Store(true)

	// Cancel the polling loop context
	m.cancelFunc()

	// Mark all tasks as cancelled and clear the map
	m.mutex.Lock()
	for _, task := range m.activeTasks {
		task.Cancelled.Store(true)
	}
	m.activeTasks = make(map[string]*MigrationTask)
	m.mutex.Unlock()

	klog.V(2).Infof("Stopped all active migration monitoring tasks")
}

// Recovery function using labels
func (d *Driver) recoverMigrationMonitorsFromLabels(ctx context.Context) error {
	labelSelector := fmt.Sprintf("%s=true", LabelMigrationInProgress)
	pvList, err := d.cloud.KubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return err
	}

	recoveredCount := 0
	for _, pv := range pvList.Items {
		if pv.Spec.ClaimRef == nil {
			klog.V(2).Infof("PV %s has no claim reference, skipping recovery", pv.Name)
			continue
		}

		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == d.Name {
			diskURI := pv.Spec.CSI.VolumeHandle

			klog.V(3).Infof("Recovering migration monitor for PV: %s", pv.Name)

			fromSKU := string(armcompute.DiskStorageAccountTypesPremiumLRS)
			toSKU := armcompute.DiskStorageAccountTypesPremiumV2LRS

			if pv.Spec.CSI.VolumeAttributes != nil {
				if sku, exists := azureutils.ParseDiskParametersForKey(pv.Spec.CSI.VolumeAttributes, "storageAccountType"); exists {
					if !strings.EqualFold(sku, string(fromSKU)) && !strings.EqualFold(sku, string(toSKU)) {
						continue
					}
				}
				if sku, exists := azureutils.ParseDiskParametersForKey(pv.Spec.CSI.VolumeAttributes, "skuName"); exists {
					if !strings.EqualFold(sku, string(fromSKU)) && !strings.EqualFold(sku, string(toSKU)) {
						continue
					}
				}
			}

			storageQtyVal := pv.Spec.Capacity[corev1.ResourceStorage]
			storageQty := &storageQtyVal
			volumeSizeInBytes := storageQty.Value()

			if err := d.migrationMonitor.StartMigrationMonitoring(ctx, false, diskURI, pv.Name, fromSKU, toSKU, volumeSizeInBytes); err != nil {
				klog.Errorf("Failed to recover migration for PV %s: %v", pv.Name, err)
			} else {
				recoveredCount++
			}
		}
	}

	klog.V(4).Infof("Recovered %d migration monitors from labels", recoveredCount)
	return nil
}
