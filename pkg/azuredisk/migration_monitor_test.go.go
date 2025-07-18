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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils"
)

func TestMigrationProgressMonitor_Integration(t *testing.T) {
	// Setup
	kubeClient := fake.NewSimpleClientset()
	eventRecorder := record.NewFakeRecorder(100)
	diskController := &ManagedDiskController{} // Mock controller

	monitor := NewMigrationProgressMonitor(kubeClient, eventRecorder, diskController)
	defer monitor.Stop()

	// Create test PV
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pv",
		},
		Spec: v1.PersistentVolumeSpec{
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("10Gi"),
			},
		},
	}
	_, err := kubeClient.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
	require.NoError(t, err)

	// Test migration monitoring
	diskURI := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Compute/disks/disk1"
	err = monitor.StartMigrationMonitoring(context.Background(), diskURI, "test-pv", "Premium_LRS", "PremiumV2_LRS")
	assert.NoError(t, err)

	// Verify task is active
	assert.True(t, monitor.IsMigrationActive(diskURI))

	// Check active migrations
	activeMigrations := monitor.GetActiveMigrations()
	assert.Len(t, activeMigrations, 1)
	assert.Contains(t, activeMigrations, diskURI)

	// Verify start event was emitted
	select {
	case event := <-eventRecorder.Events:
		assert.Contains(t, event, "SKUMigrationStarted")
		assert.Contains(t, event, "Premium_LRS")
		assert.Contains(t, event, "PremiumV2_LRS")
	case <-time.After(1 * time.Second):
		t.Fatal("Expected start event was not recorded")
	}
}

func TestMigrationProgressMonitor_ParseDiskURI(t *testing.T) {
	testCases := []struct {
		name             string
		diskURI          string
		expectedRG       string
		expectedDiskName string
		wantError        bool
	}{
		{
			name:             "valid_disk_uri",
			diskURI:          "/subscriptions/sub1/resourceGroups/myRG/providers/Microsoft.Compute/disks/myDisk",
			expectedRG:       "myRG",
			expectedDiskName: "myDisk",
			wantError:        false,
		},
		{
			name:      "invalid_short_uri",
			diskURI:   "/invalid/uri",
			wantError: true,
		},
		{
			name:      "missing_components",
			diskURI:   "/subscriptions/sub1/providers/Microsoft.Compute/disks/myDisk",
			wantError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, rg, diskName, err := azureutils.GetInfoFromURI(tc.diskURI)

			if tc.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectedRG, rg)
				assert.Equal(t, tc.expectedDiskName, diskName)
			}
		})
	}
}

func TestMigrationProgressMonitor_ShouldReportProgress(t *testing.T) {
	monitor := &MigrationProgressMonitor{}

	testCases := []struct {
		name     string
		current  float32
		last     float32
		expected bool
	}{
		{"first_milestone_20", 20, 0, true},
		{"progress_within_threshold", 25, 20, false},
		{"next_milestone_40", 40, 20, true},
		{"completion_100", 100, 80, true},
		{"no_progress", 20, 20, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := monitor.shouldReportProgress(tc.current, tc.last)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestMigrationProgressMonitor_DuplicatePrevention(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	eventRecorder := record.NewFakeRecorder(100)
	diskController := &ManagedDiskController{}

	monitor := NewMigrationProgressMonitor(kubeClient, eventRecorder, diskController)
	defer monitor.Stop()

	diskURI := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Compute/disks/disk1"

	// Start first monitoring
	err1 := monitor.StartMigrationMonitoring(context.Background(), diskURI, "test-pv", "Premium_LRS", "PremiumV2_LRS")
	assert.NoError(t, err1)

	// Attempt duplicate monitoring
	err2 := monitor.StartMigrationMonitoring(context.Background(), diskURI, "test-pv", "Premium_LRS", "PremiumV2_LRS")
	assert.NoError(t, err2) // Should not error, just skip

	// Verify only one task exists
	activeMigrations := monitor.GetActiveMigrations()
	assert.Len(t, activeMigrations, 1)
}
