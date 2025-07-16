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
	"encoding/json"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils"
)

// Test constants
const (
	testConfigMapName = "azuredisk-sku-migration-config"
	testNamespace     = "kube-system"
	eastUSRegion      = "eastus"
	centralUSRegion   = "centralus"
	westUS2Region     = "westus2"
)

// TestSKUMigrationValidator_ValidateMigration tests the static validator
func TestSKUMigrationValidator_ValidateMigration(t *testing.T) {
	validator := NewSKUMigrationValidator()

	testCases := []struct {
		name           string
		disk           *armcompute.Disk
		fromSKU        armcompute.DiskStorageAccountTypes
		toSKU          armcompute.DiskStorageAccountTypes
		diskParams     azureutils.ManagedDiskParameters
		expectError    bool
		expectedErrMsg string
	}{
		{
			name:    "successful_migration_with_compatible_settings",
			disk:    createTestDisk(eastUSRegion, "None", false, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			diskParams: azureutils.ManagedDiskParameters{
				AccountType:        "PremiumV2_LRS",
				CachingMode:        v1.AzureDataDiskCachingNone,
				EnableBursting:     toBoolPtr(false),
				LogicalSectorSize:  512,
				DiskEncryptionType: "EncryptionAtRestWithPlatformKey",
			},
			expectError: false,
		},
		{
			name:    "validation_failure_unsupported_region",
			disk:    createTestDisk(centralUSRegion, "None", false, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			diskParams: azureutils.ManagedDiskParameters{
				AccountType: "PremiumV2_LRS",
			},
			expectError:    true,
			expectedErrMsg: "RegionSupport validation failed",
		},
		{
			name:    "validation_failure_invalid_caching_mode",
			disk:    createTestDisk(eastUSRegion, "ReadWrite", false, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			diskParams: azureutils.ManagedDiskParameters{
				AccountType: "PremiumV2_LRS",
				CachingMode: v1.AzureDataDiskCachingReadWrite,
			},
			expectError:    true,
			expectedErrMsg: "CachingMode validation failed",
		},
		{
			name:    "validation_failure_bursting_enabled",
			disk:    createTestDisk(eastUSRegion, "None", true, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			diskParams: azureutils.ManagedDiskParameters{
				AccountType:    "PremiumV2_LRS",
				EnableBursting: toBoolPtr(true),
			},
			expectError:    true,
			expectedErrMsg: "EnableBursting validation failed",
		},
		{
			name:    "validation_failure_double_encryption",
			disk:    createTestDisk(eastUSRegion, "None", false, 512, "EncryptionAtRestWithPlatformAndCustomerKeys"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			diskParams: azureutils.ManagedDiskParameters{
				AccountType:        "PremiumV2_LRS",
				DiskEncryptionType: "EncryptionAtRestWithPlatformAndCustomerKeys",
			},
			expectError:    true,
			expectedErrMsg: "DiskEncryptionType validation failed",
		},
		{
			name:    "validation_failure_invalid_logical_sector_size",
			disk:    createTestDisk(eastUSRegion, "None", false, 4096, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			diskParams: azureutils.ManagedDiskParameters{
				AccountType:       "PremiumV2_LRS",
				LogicalSectorSize: 4096,
			},
			expectError:    true,
			expectedErrMsg: "LogicalSectorSize validation failed",
		},
		{
			name:    "validation_failure_unsupported_migration_path",
			disk:    createTestDisk(eastUSRegion, "None", false, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesStandardLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			diskParams: azureutils.ManagedDiskParameters{
				AccountType: "PremiumV2_LRS",
			},
			expectError:    true,
			expectedErrMsg: "migration from Standard_LRS to PremiumV2_LRS is not supported",
		},
		{
			name:    "successful_migration_with_performance_parameters",
			disk:    createTestDisk(eastUSRegion, "None", false, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			diskParams: azureutils.ManagedDiskParameters{
				AccountType:        "PremiumV2_LRS",
				CachingMode:        v1.AzureDataDiskCachingNone,
				EnableBursting:     toBoolPtr(false),
				LogicalSectorSize:  512,
				DiskIOPSReadWrite:  "3000",
				DiskMBPSReadWrite:  "125",
				DiskEncryptionType: "EncryptionAtRestWithPlatformKey",
			},
			expectError: false,
		},
		{
			name:    "successful_migration_minimal_parameters",
			disk:    createTestDisk(eastUSRegion, "None", false, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			diskParams: azureutils.ManagedDiskParameters{
				AccountType: "PremiumV2_LRS",
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validator.ValidateMigration(context.Background(), tc.disk, tc.fromSKU, tc.toSKU, tc.diskParams)

			if tc.expectError {
				assert.Error(t, err)
				if tc.expectedErrMsg != "" {
					assert.Contains(t, err.Error(), tc.expectedErrMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestConfigurableSKUMigrationValidator_ValidateMigration tests the configurable validator
func TestConfigurableSKUMigrationValidator_ValidateMigration(t *testing.T) {
	testCases := []struct {
		name           string
		disk           *armcompute.Disk
		fromSKU        armcompute.DiskStorageAccountTypes
		toSKU          armcompute.DiskStorageAccountTypes
		config         *MigrationConfig
		diskParams     azureutils.ManagedDiskParameters
		expectError    bool
		expectedErrMsg string
	}{
		{
			name:    "successful_validation_all_rules_enabled",
			disk:    createTestDisk(eastUSRegion, "None", false, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			config:  createValidMigrationConfig(),
			diskParams: azureutils.ManagedDiskParameters{
				AccountType:        "PremiumV2_LRS",
				CachingMode:        v1.AzureDataDiskCachingNone,
				EnableBursting:     toBoolPtr(false),
				LogicalSectorSize:  512,
				DiskEncryptionType: "EncryptionAtRestWithPlatformKey",
			},
			expectError: false,
		},
		{
			name:    "validation_failure_unsupported_region_restrictive_config",
			disk:    createTestDisk(centralUSRegion, "None", false, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			config:  createRestrictiveMigrationConfig(),
			diskParams: azureutils.ManagedDiskParameters{
				AccountType: "PremiumV2_LRS",
			},
			expectError:    true,
			expectedErrMsg: "RegionSupport validation failed",
		},
		{
			name:    "successful_validation_rules_disabled",
			disk:    createTestDisk(centralUSRegion, "ReadWrite", true, 4096, "EncryptionAtRestWithPlatformAndCustomerKeys"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			config:  createPermissiveMigrationConfig(),
			diskParams: azureutils.ManagedDiskParameters{
				AccountType:        "PremiumV2_LRS",
				CachingMode:        v1.AzureDataDiskCachingReadWrite,
				EnableBursting:     toBoolPtr(true),
				LogicalSectorSize:  4096,
				DiskEncryptionType: "EncryptionAtRestWithPlatformAndCustomerKeys",
			},
			expectError: false,
		},
		{
			name:    "validation_failure_migration_disabled",
			disk:    createTestDisk(eastUSRegion, "None", false, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			config:  createDisabledMigrationConfig(),
			diskParams: azureutils.ManagedDiskParameters{
				AccountType: "PremiumV2_LRS",
			},
			expectError:    true,
			expectedErrMsg: "migration from Premium_LRS to PremiumV2_LRS is disabled",
		},
		{
			name:    "successful_validation_custom_region_config",
			disk:    createTestDisk(westUS2Region, "None", false, 512, "EncryptionAtRestWithPlatformKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			config:  createValidMigrationConfig(),
			diskParams: azureutils.ManagedDiskParameters{
				AccountType: "PremiumV2_LRS",
				CachingMode: v1.AzureDataDiskCachingNone,
			},
			expectError: false,
		},
		{
			name:    "validation_with_performance_parameters",
			disk:    createTestDisk(eastUSRegion, "None", false, 512, "EncryptionAtRestWithCustomerKey"),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			config:  createValidMigrationConfig(),
			diskParams: azureutils.ManagedDiskParameters{
				AccountType:        "PremiumV2_LRS",
				CachingMode:        v1.AzureDataDiskCachingNone,
				EnableBursting:     toBoolPtr(false),
				LogicalSectorSize:  512,
				DiskIOPSReadWrite:  "5000",
				DiskMBPSReadWrite:  "200",
				DiskEncryptionType: "EncryptionAtRestWithCustomerKey",
			},
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset()
			configManager := createMockConfigManager(kubeClient, tc.config)
			validator := NewConfigurableSKUMigrationValidator(configManager)

			err := validator.ValidateMigration(context.Background(), tc.disk, tc.fromSKU, tc.toSKU, tc.diskParams)

			if tc.expectError {
				assert.Error(t, err)
				if tc.expectedErrMsg != "" {
					assert.Contains(t, err.Error(), tc.expectedErrMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestConfigManager_LoadConfig tests configuration loading from ConfigMaps
func TestConfigManager_LoadConfig(t *testing.T) {
	testCases := []struct {
		name               string
		configMap          *v1.ConfigMap
		expectError        bool
		expectedRulesCount int
	}{
		{
			name:               "successful_config_load",
			configMap:          createTestConfigMap(),
			expectError:        false,
			expectedRulesCount: 6,
		},
		{
			name:               "configmap_not_found_use_defaults",
			configMap:          nil,
			expectError:        false,
			expectedRulesCount: 6,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset()
			if tc.configMap != nil {
				_, err := kubeClient.CoreV1().ConfigMaps(testNamespace).Create(context.TODO(), tc.configMap, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			configManager := NewSkuMigrationConfigManager(kubeClient, testConfigMapName, testNamespace)
			err := configManager.LoadConfig()

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				migrationConfig, err := configManager.GetMigrationConfig("Premium_LRS", "PremiumV2_LRS")
				assert.NoError(t, err)
				assert.Len(t, migrationConfig.ValidationRules, tc.expectedRulesCount)
			}
		})
	}
}

// TestIndividualValidationRules tests each validation rule individually
func TestIndividualValidationRules(t *testing.T) {
	testCases := []struct {
		name       string
		rule       ConfigurableValidationRule
		disk       *armcompute.Disk
		fromSKU    armcompute.DiskStorageAccountTypes
		toSKU      armcompute.DiskStorageAccountTypes
		config     ValidationRuleConfig
		diskParams azureutils.ManagedDiskParameters
		wantError  bool
	}{
		{
			name:    "region_support_valid_region",
			rule:    &ConfigurableRegionSupportRule{},
			disk:    createTestDisk(eastUSRegion, "None", false, 512, ""),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			config: ValidationRuleConfig{
				Enabled: true,
				Params: map[string]interface{}{
					"supportedRegions": []string{eastUSRegion, westUS2Region},
				},
			},
			diskParams: azureutils.ManagedDiskParameters{},
			wantError:  false,
		},
		{
			name:    "region_support_invalid_region",
			rule:    &ConfigurableRegionSupportRule{},
			disk:    createTestDisk(centralUSRegion, "None", false, 512, ""),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			config: ValidationRuleConfig{
				Enabled: true,
				Params: map[string]interface{}{
					"supportedRegions": []string{eastUSRegion, westUS2Region},
				},
			},
			diskParams: azureutils.ManagedDiskParameters{},
			wantError:  true,
		},
		{
			name:    "caching_mode_valid_mode",
			rule:    &ConfigurableCachingModeRule{},
			disk:    createTestDisk(eastUSRegion, "None", false, 512, ""),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			config: ValidationRuleConfig{
				Enabled: true,
				Params: map[string]interface{}{
					"allowedModes": []string{"None"},
				},
			},
			diskParams: azureutils.ManagedDiskParameters{
				CachingMode: v1.AzureDataDiskCachingNone,
			},
			wantError: false,
		},
		{
			name:    "enable_bursting_disabled_correctly",
			rule:    &ConfigurableEnableBurstingRule{},
			disk:    createTestDisk(eastUSRegion, "None", false, 512, ""),
			fromSKU: armcompute.DiskStorageAccountTypesPremiumLRS,
			toSKU:   armcompute.DiskStorageAccountTypesPremiumV2LRS,
			config: ValidationRuleConfig{
				Enabled: true,
				Params: map[string]interface{}{
					"allowBursting": false,
				},
			},
			diskParams: azureutils.ManagedDiskParameters{
				EnableBursting: toBoolPtr(false),
			},
			wantError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.ValidateWithConfig(context.Background(), tc.disk, tc.fromSKU, tc.toSKU, tc.config, tc.diskParams)

			if tc.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// Helper functions for creating test data

// createTestDisk creates a test disk with specified properties
func createTestDisk(location, cachingMode string, burstingEnabled bool, sectorSize int32, encryptionType string) *armcompute.Disk {
	disk := &armcompute.Disk{
		Location: &location,
		Properties: &armcompute.DiskProperties{
			BurstingEnabled: &burstingEnabled,
			CreationData: &armcompute.CreationData{
				LogicalSectorSize: &sectorSize,
			},
		},
	}

	if encryptionType != "" {
		disk.Properties.Encryption = &armcompute.Encryption{
			Type: (*armcompute.EncryptionType)(&encryptionType),
		}
	}

	return disk
}

// createValidMigrationConfig creates a valid migration configuration for testing
func createValidMigrationConfig() *MigrationConfig {
	return &MigrationConfig{
		MigrationPairs: map[string]*MigrationPairConfig{
			"Premium_LRS->PremiumV2_LRS": {
				Enabled: true,
				ValidationRules: map[string]*ValidationRuleConfig{
					"RegionSupport": {
						Enabled: true,
						Params: map[string]interface{}{
							"supportedRegions": []string{eastUSRegion, westUS2Region},
						},
					},
					"CachingMode": {
						Enabled: true,
						Params: map[string]interface{}{
							"allowedModes": []string{"None"},
						},
					},
					"EnableBursting": {
						Enabled: true,
						Params: map[string]interface{}{
							"allowBursting": false,
						},
					},
					"DiskEncryptionType": {
						Enabled: true,
						Params: map[string]interface{}{
							"allowedTypes": []string{"EncryptionAtRestWithPlatformKey", "EncryptionAtRestWithCustomerKey"},
						},
					},
					"LogicalSectorSize": {
						Enabled: true,
						Params: map[string]interface{}{
							"requiredSize": 512,
						},
					},
					"PerfProfile": {
						Enabled: true,
						Params: map[string]interface{}{
							"allowedProfiles": []string{"", "basic"},
						},
					},
				},
			},
		},
	}
}

// createRestrictiveMigrationConfig creates a restrictive migration configuration
func createRestrictiveMigrationConfig() *MigrationConfig {
	return &MigrationConfig{
		MigrationPairs: map[string]*MigrationPairConfig{
			"Premium_LRS->PremiumV2_LRS": {
				Enabled: true,
				ValidationRules: map[string]*ValidationRuleConfig{
					"RegionSupport": {
						Enabled: true,
						Params: map[string]interface{}{
							"supportedRegions": []string{eastUSRegion}, // Only eastus allowed
						},
					},
					"CachingMode":        {Enabled: true},
					"EnableBursting":     {Enabled: true},
					"DiskEncryptionType": {Enabled: true},
					"LogicalSectorSize":  {Enabled: true},
					"PerfProfile":        {Enabled: true},
				},
			},
		},
	}
}

// createPermissiveMigrationConfig creates a permissive migration configuration
func createPermissiveMigrationConfig() *MigrationConfig {
	return &MigrationConfig{
		MigrationPairs: map[string]*MigrationPairConfig{
			"Premium_LRS->PremiumV2_LRS": {
				Enabled: true,
				ValidationRules: map[string]*ValidationRuleConfig{
					"RegionSupport":      {Enabled: false}, // All rules disabled
					"CachingMode":        {Enabled: false},
					"EnableBursting":     {Enabled: false},
					"DiskEncryptionType": {Enabled: false},
					"LogicalSectorSize":  {Enabled: false},
					"PerfProfile":        {Enabled: false},
				},
			},
		},
	}
}

// createDisabledMigrationConfig creates a disabled migration configuration
func createDisabledMigrationConfig() *MigrationConfig {
	return &MigrationConfig{
		MigrationPairs: map[string]*MigrationPairConfig{
			"Premium_LRS->PremiumV2_LRS": {
				Enabled:         false, // Migration disabled
				ValidationRules: map[string]*ValidationRuleConfig{},
			},
		},
	}
}

// createTestConfigMap creates a test ConfigMap with migration configuration
func createTestConfigMap() *v1.ConfigMap {
	config := createValidMigrationConfig()
	configJSON, _ := json.Marshal(config)

	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testConfigMapName,
			Namespace: testNamespace,
		},
		Data: map[string]string{
			"config.json": string(configJSON),
		},
	}
}

// createMockConfigManager creates a mock configuration manager for testing
func createMockConfigManager(kubeClient *fake.Clientset, config *MigrationConfig) *SkuMigrationConfigManager {
	configManager := NewSkuMigrationConfigManager(kubeClient, testConfigMapName, testNamespace)
	configManager.config = config
	return configManager
}

// toBoolPtr converts a boolean value to a pointer
func toBoolPtr(b bool) *bool {
	return &b
}
