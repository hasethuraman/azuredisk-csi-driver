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
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"sigs.k8s.io/azuredisk-csi-driver/pkg/azuredisk/mockcorev1"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azuredisk/mockkubeclient"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azuredisk/mockpersistentvolume"
)

func TestNewMigrationProgressMonitor(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockKubeClient := mockkubeclient.NewMockInterface(ctrl)
	mockEventRecorder := record.NewFakeRecorder(10)
	mockDiskController := &ManagedDiskController{}

	monitor := NewMigrationProgressMonitor(mockKubeClient, mockEventRecorder, mockDiskController)

	assert.NotNil(t, monitor)
	assert.Equal(t, mockKubeClient, monitor.kubeClient)
	assert.Equal(t, mockEventRecorder, monitor.eventRecorder)
	assert.Equal(t, mockDiskController, monitor.diskController)
	assert.NotNil(t, monitor.activeTasks)
	assert.Equal(t, 0, len(monitor.activeTasks))
}

func TestStartMigrationMonitoring(t *testing.T) {
	tests := []struct {
		name        string
		diskURI     string
		pvName      string
		fromSKU     armcompute.DiskStorageAccountTypes
		toSKU       armcompute.DiskStorageAccountTypes
		expectError bool
	}{
		{
			name:        "successful start Premium_LRS to PremiumV2_LRS",
			diskURI:     "/subscriptions/test/resourceGroups/rg/providers/Microsoft.Compute/disks/test-disk",
			pvName:      "test-pv",
			fromSKU:     armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:       armcompute.DiskStorageAccountTypesPremiumV2LRS,
			expectError: false,
		},
		{
			name:        "successful start Standard_LRS to Premium_LRS",
			diskURI:     "/subscriptions/test/resourceGroups/rg/providers/Microsoft.Compute/disks/test-disk-2",
			pvName:      "test-pv-2",
			fromSKU:     armcompute.DiskStorageAccountTypesStandardLRS,
			toSKU:       armcompute.DiskStorageAccountTypesPremiumLRS,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockKubeClient := mockkubeclient.NewMockInterface(ctrl)
			mockEventRecorder := record.NewFakeRecorder(10)
			mockDiskController := &ManagedDiskController{}

			monitor := NewMigrationProgressMonitor(mockKubeClient, mockEventRecorder, mockDiskController)

			ctx := context.Background()
			err := monitor.StartMigrationMonitoring(ctx, tt.diskURI, tt.pvName, tt.fromSKU, tt.toSKU)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.True(t, monitor.IsMigrationActive(tt.diskURI))

				// Verify task was created correctly
				activeTasks := monitor.GetActiveMigrations()
				assert.Equal(t, 1, len(activeTasks))

				task, exists := activeTasks[tt.diskURI]
				assert.True(t, exists)
				assert.Equal(t, tt.diskURI, task.DiskURI)
				assert.Equal(t, tt.pvName, task.PVName)
				assert.Equal(t, tt.fromSKU, task.FromSKU)
				assert.Equal(t, tt.toSKU, task.ToSKU)
				assert.NotNil(t, task.CancelFunc)
				assert.NotNil(t, task.Context)

				// Stop monitoring to clean up
				monitor.Stop()
			}
		})
	}
}

func TestIsMigrationActive(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockKubeClient := mockkubeclient.NewMockInterface(ctrl)
	mockEventRecorder := record.NewFakeRecorder(10)
	mockDiskController := &ManagedDiskController{}

	monitor := NewMigrationProgressMonitor(mockKubeClient, mockEventRecorder, mockDiskController)

	diskURI := "/subscriptions/test/resourceGroups/rg/providers/Microsoft.Compute/disks/test-disk"
	nonExistentDiskURI := "/subscriptions/test/resourceGroups/rg/providers/Microsoft.Compute/disks/non-existent"

	// Initially no migration should be active
	assert.False(t, monitor.IsMigrationActive(diskURI))
	assert.False(t, monitor.IsMigrationActive(nonExistentDiskURI))

	// Start monitoring
	ctx := context.Background()
	err := monitor.StartMigrationMonitoring(ctx, diskURI, "test-pv",
		armcompute.DiskStorageAccountTypesPremiumLRS,
		armcompute.DiskStorageAccountTypesPremiumV2LRS)
	assert.NoError(t, err)

	// Now should be active
	assert.True(t, monitor.IsMigrationActive(diskURI))
	assert.False(t, monitor.IsMigrationActive(nonExistentDiskURI))

	monitor.Stop()

	// After stop, should not be active anymore
	time.Sleep(100 * time.Millisecond) // Allow goroutines to finish
	assert.False(t, monitor.IsMigrationActive(diskURI))
}

func TestShouldReportProgress(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockKubeClient := mockkubeclient.NewMockInterface(ctrl)
	mockEventRecorder := record.NewFakeRecorder(10)
	mockDiskController := &ManagedDiskController{}

	monitor := NewMigrationProgressMonitor(mockKubeClient, mockEventRecorder, mockDiskController)

	tests := []struct {
		name     string
		current  float32
		last     float32
		expected bool
	}{
		{"initial progress", 15.0, 0.0, false},
		{"reach 20% milestone", 20.0, 15.0, true},
		{"within same milestone", 25.0, 20.0, false},
		{"reach 40% milestone", 40.0, 25.0, true},
		{"reach 60% milestone", 60.0, 45.0, true},
		{"reach 80% milestone", 80.0, 65.0, true},
		{"completion", 100.0, 85.0, true},
		{"regression", 75.0, 80.0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := monitor.shouldReportProgress(tt.current, tt.last)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestEmitMigrationEvent(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockKubeClient := mockkubeclient.NewMockInterface(ctrl)
	mockCoreV1 := mockcorev1.NewMockInterface(ctrl)
	mockPVInterface := mockpersistentvolume.NewMockInterface(ctrl)
	mockEventRecorder := record.NewFakeRecorder(10)
	mockDiskController := &ManagedDiskController{}

	monitor := NewMigrationProgressMonitor(mockKubeClient, mockEventRecorder, mockDiskController)

	// Create test PV
	testPV := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pv",
		},
	}

	// Create test task
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task := &MigrationTask{
		DiskURI:    "/subscriptions/test/resourceGroups/rg/providers/Microsoft.Compute/disks/test-disk",
		PVName:     "test-pv",
		FromSKU:    armcompute.DiskStorageAccountTypesPremiumLRS,
		ToSKU:      armcompute.DiskStorageAccountTypesPremiumV2LRS,
		StartTime:  time.Now(),
		Context:    ctx,
		CancelFunc: cancel,
	}

	// Set up mocks
	mockKubeClient.EXPECT().CoreV1().Return(mockCoreV1)
	mockCoreV1.EXPECT().PersistentVolumes().Return(mockPVInterface)
	mockPVInterface.EXPECT().Get(gomock.Any(), "test-pv", gomock.Any()).Return(testPV, nil)

	// Test successful event emission
	monitor.emitMigrationEvent(task, EventTypeNormal, ReasonSKUMigrationStarted, "Test migration started")

	// Verify event was recorded
	select {
	case event := <-mockEventRecorder.Events:
		assert.Contains(t, event, "Normal")
		assert.Contains(t, event, ReasonSKUMigrationStarted)
		assert.Contains(t, event, "Test migration started")
	default:
		t.Error("Expected event was not recorded")
	}
}

func TestEmitMigrationEvent_PVNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockKubeClient := mockkubeclient.NewMockInterface(ctrl)
	mockCoreV1 := mockcorev1.NewMockInterface(ctrl)
	mockPVInterface := mockpersistentvolume.NewMockInterface(ctrl)
	mockEventRecorder := record.NewFakeRecorder(10)
	mockDiskController := &ManagedDiskController{}

	monitor := NewMigrationProgressMonitor(mockKubeClient, mockEventRecorder, mockDiskController)

	// Create test task
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task := &MigrationTask{
		DiskURI:    "/subscriptions/test/resourceGroups/rg/providers/Microsoft.Compute/disks/test-disk",
		PVName:     "non-existent-pv",
		FromSKU:    armcompute.DiskStorageAccountTypesPremiumLRS,
		ToSKU:      armcompute.DiskStorageAccountTypesPremiumV2LRS,
		StartTime:  time.Now(),
		Context:    ctx,
		CancelFunc: cancel,
	}

	// Set up mocks to return error
	mockKubeClient.EXPECT().CoreV1().Return(mockCoreV1)
	mockCoreV1.EXPECT().PersistentVolumes().Return(mockPVInterface)
	mockPVInterface.EXPECT().Get(gomock.Any(), "non-existent-pv", gomock.Any()).Return(nil, errors.New("not found"))

	// Test event emission with PV not found - should not panic
	monitor.emitMigrationEvent(task, EventTypeNormal, ReasonSKUMigrationStarted, "Test migration started")

	// Verify no event was recorded
	select {
	case <-mockEventRecorder.Events:
		t.Error("No event should have been recorded when PV is not found")
	default:
		// Expected - no event should be recorded
	}
}

func TestStop(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockKubeClient := mockkubeclient.NewMockInterface(ctrl)
	mockEventRecorder := record.NewFakeRecorder(10)
	mockDiskController := &ManagedDiskController{}

	monitor := NewMigrationProgressMonitor(mockKubeClient, mockEventRecorder, mockDiskController)

	// Start multiple migrations
	ctx := context.Background()
	diskURIs := []string{
		"/subscriptions/test/resourceGroups/rg/providers/Microsoft.Compute/disks/disk1",
		"/subscriptions/test/resourceGroups/rg/providers/Microsoft.Compute/disks/disk2",
		"/subscriptions/test/resourceGroups/rg/providers/Microsoft.Compute/disks/disk3",
	}

	for i, diskURI := range diskURIs {
		err := monitor.StartMigrationMonitoring(ctx, diskURI, fmt.Sprintf("pv-%d", i+1),
			armcompute.DiskStorageAccountTypesPremiumLRS,
			armcompute.DiskStorageAccountTypesPremiumV2LRS)
		assert.NoError(t, err)
	}

	// Verify migrations are active
	activeTasks := monitor.GetActiveMigrations()
	assert.Equal(t, 3, len(activeTasks))

	// Stop all migrations
	monitor.Stop()

	// Allow some time for goroutines to finish
	time.Sleep(100 * time.Millisecond)

	// Verify all migrations are stopped
	for _, diskURI := range diskURIs {
		assert.False(t, monitor.IsMigrationActive(diskURI))
	}

	activeTasks = monitor.GetActiveMigrations()
	assert.Equal(t, 0, len(activeTasks))
}
