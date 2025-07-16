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
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils"
)

// ConfigurableValidationRule defines the interface for configurable validation rules
type ConfigurableValidationRule interface {
	ValidateWithConfig(ctx context.Context, disk *armcompute.Disk, fromSKU, toSKU armcompute.DiskStorageAccountTypes, config ValidationRuleConfig, diskParams azureutils.ManagedDiskParameters) error
	Name() string
}

// ConfigurableSKUMigrationValidator implements SKUMigrationValidator with configurable rules
type ConfigurableSKUMigrationValidator struct {
	configManager *SkuMigrationConfigManager
	rules         map[string]ConfigurableValidationRule
}

// NewConfigurableSKUMigrationValidator creates a new configurable SKU migration validator
func NewConfigurableSKUMigrationValidator(configManager *SkuMigrationConfigManager) ConfigurableSKUMigrationValidator {
	rules := map[string]ConfigurableValidationRule{
		"RegionSupport":      &ConfigurableRegionSupportRule{},
		"CachingMode":        &ConfigurableCachingModeRule{},
		"EnableBursting":     &ConfigurableEnableBurstingRule{},
		"DiskEncryptionType": &ConfigurableDiskEncryptionTypeRule{},
		"LogicalSectorSize":  &ConfigurableLogicalSectorSizeRule{},
		"PerfProfile":        &ConfigurablePerfProfileRule{},
	}

	return ConfigurableSKUMigrationValidator{
		configManager: configManager,
		rules:         rules,
	}
}

// NewSKUMigrationValidator creates a validator with default config (no ConfigMap needed)
func NewSKUMigrationValidator() ConfigurableSKUMigrationValidator {
	// Create a config manager with default settings (no Kubernetes client)
	configManager := &SkuMigrationConfigManager{
		config: getDefaultMigrationConfig(),
	}

	return NewConfigurableSKUMigrationValidator(configManager)
}

// ValidateMigration validates a SKU migration using configurable rules
func (v *ConfigurableSKUMigrationValidator) ValidateMigration(ctx context.Context, disk *armcompute.Disk, fromSKU, toSKU armcompute.DiskStorageAccountTypes, diskParams azureutils.ManagedDiskParameters) error {
	migrationKey := fmt.Sprintf("%s->%s", fromSKU, toSKU)
	klog.V(2).Infof("Starting configurable SKU migration validation: %s", migrationKey)

	migrationConfig, err := v.configManager.GetMigrationConfig(string(fromSKU), string(toSKU))
	if err != nil {
		return fmt.Errorf("failed to get migration configuration: %w", err)
	}

	if !migrationConfig.Enabled {
		return fmt.Errorf("migration from %s to %s is disabled", fromSKU, toSKU)
	}

	var validationErrors []error

	for ruleName, rule := range v.rules {
		ruleConfig, exists := migrationConfig.ValidationRules[ruleName]
		if !exists {
			// Use default configuration if not specified
			ruleConfig = &ValidationRuleConfig{Enabled: true, Params: make(map[string]interface{})}
		}

		if !ruleConfig.Enabled {
			klog.V(4).Infof("Skipping disabled validation rule: %s", ruleName)
			continue
		}

		klog.V(4).Infof("Running configurable validation rule: %s", ruleName)
		if err := rule.ValidateWithConfig(ctx, disk, fromSKU, toSKU, *ruleConfig, diskParams); err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("%s validation failed: %w", ruleName, err))
		}
	}

	if len(validationErrors) > 0 {
		return fmt.Errorf("validation failed: %v", validationErrors)
	}

	klog.V(2).Infof("Configurable SKU migration validation successful: %s", migrationKey)
	return nil
}

// ConfigurableRegionSupportRule validates region support with configuration
type ConfigurableRegionSupportRule struct{}

func (r *ConfigurableRegionSupportRule) ValidateWithConfig(ctx context.Context, disk *armcompute.Disk, fromSKU, toSKU armcompute.DiskStorageAccountTypes, config ValidationRuleConfig, diskParams azureutils.ManagedDiskParameters) error {
	if disk.Location == nil {
		return fmt.Errorf("disk location is not specified")
	}

	supportedRegions, err := r.getSupportedRegions(config)
	if err != nil {
		return err
	}

	diskLocation := strings.ToLower(*disk.Location)

	// Check if "*" (all regions) is specified
	for _, region := range supportedRegions {
		if region == "*" {
			return nil
		}
		if strings.ToLower(region) == diskLocation {
			return nil
		}
	}

	return fmt.Errorf("disk region '%s' is not supported for %s migration", diskLocation, toSKU)
}

func (r *ConfigurableRegionSupportRule) Name() string {
	return "RegionSupport"
}

func (r *ConfigurableRegionSupportRule) getSupportedRegions(config ValidationRuleConfig) ([]string, error) {
	if regions, exists := config.Params["supportedRegions"]; exists {
		if regionList, ok := regions.([]interface{}); ok {
			var result []string
			for _, region := range regionList {
				if regionStr, ok := region.(string); ok {
					result = append(result, regionStr)
				}
			}
			return result, nil
		}
	}

	// Return default supported regions for PremiumV2_LRS
	// from https://learn.microsoft.com/en-us/azure/virtual-machines/disks-deploy-premium-v2?tabs=azure-cli#regional-availability
	return []string{
		"australiacentral2", "australiaeast", "australiasoutheast", "brazilsouth",
		"canadacentral", "canadaeast", "centralindia", "centralus", "chinanorth3",
		"eastasia", "eastus", "eastus2", "francecentral", "germanywestcentral",
		"indonesiacentral", "israelcentral", "italynorth", "japaneast", "japanwest",
		"koreacentral", "malaysiawest", "mexicocentral", "newzealandnorth",
		"northcentralus", "northeurope", "norwayeast", "norwaywest", "polandcentral",
		"southafricanorth", "southcentralus", "southeastasia", "spaincentral",
		"swedencentral", "switzerlandnorth", "taiwannorth", "uaenorth", "uksouth",
		"ukwest", "usgovvirginia", "westcentralus", "westeurope", "westus", "westus2",
		"westus3",
	}, nil
}

// ConfigurableCachingModeRule validates caching mode with configuration
type ConfigurableCachingModeRule struct{}

func (r *ConfigurableCachingModeRule) ValidateWithConfig(ctx context.Context, disk *armcompute.Disk, fromSKU, toSKU armcompute.DiskStorageAccountTypes, config ValidationRuleConfig, diskParams azureutils.ManagedDiskParameters) error {
	allowedModes, err := r.getAllowedCachingModes(config)
	if err != nil {
		return err
	}

	currentMode := diskParams.CachingMode
	if !r.isModeAllowed(currentMode, allowedModes) {
		return fmt.Errorf("current caching mode '%s' is not supported for %s migration", currentMode, toSKU)
	}

	return nil
}

func (r *ConfigurableCachingModeRule) Name() string {
	return "CachingMode"
}

func (r *ConfigurableCachingModeRule) getAllowedCachingModes(config ValidationRuleConfig) ([]v1.AzureDataDiskCachingMode, error) {
	if modes, exists := config.Params["allowedModes"]; exists {
		if modeList, ok := modes.([]interface{}); ok {
			var result []v1.AzureDataDiskCachingMode
			for _, mode := range modeList {
				if modeStr, ok := mode.(string); ok {
					result = append(result, v1.AzureDataDiskCachingMode(modeStr))
				}
			}
			return result, nil
		}
	}

	// Default: PremiumV2_LRS only supports None caching
	return []v1.AzureDataDiskCachingMode{v1.AzureDataDiskCachingNone}, nil
}

func (r *ConfigurableCachingModeRule) isModeAllowed(mode v1.AzureDataDiskCachingMode, allowedModes []v1.AzureDataDiskCachingMode) bool {
	for _, allowedMode := range allowedModes {
		if mode == allowedMode {
			return true
		}
	}
	return false
}

// ConfigurableEnableBurstingRule validates bursting settings with configuration
type ConfigurableEnableBurstingRule struct{}

func (r *ConfigurableEnableBurstingRule) ValidateWithConfig(ctx context.Context, disk *armcompute.Disk, fromSKU, toSKU armcompute.DiskStorageAccountTypes, config ValidationRuleConfig, diskParams azureutils.ManagedDiskParameters) error {
	allowBursting := r.getAllowBursting(config)

	if disk.Properties != nil && disk.Properties.BurstingEnabled != nil && *disk.Properties.BurstingEnabled {
		if !allowBursting {
			return fmt.Errorf("bursting must be disabled for %s migration", toSKU)
		}
	}

	return nil
}

func (r *ConfigurableEnableBurstingRule) Name() string {
	return "EnableBursting"
}

func (r *ConfigurableEnableBurstingRule) getAllowBursting(config ValidationRuleConfig) bool {
	if allow, exists := config.Params["allowBursting"]; exists {
		if allowBool, ok := allow.(bool); ok {
			return allowBool
		}
	}
	// Default: PremiumV2_LRS does not support bursting
	return false
}

// ConfigurableDiskEncryptionTypeRule validates encryption type with configuration
type ConfigurableDiskEncryptionTypeRule struct{}

func (r *ConfigurableDiskEncryptionTypeRule) ValidateWithConfig(ctx context.Context, disk *armcompute.Disk, fromSKU, toSKU armcompute.DiskStorageAccountTypes, config ValidationRuleConfig, diskParams azureutils.ManagedDiskParameters) error {
	allowedTypes, err := r.getAllowedEncryptionTypes(config)
	if err != nil {
		return err
	}

	if disk.Properties != nil && disk.Properties.Encryption != nil && disk.Properties.Encryption.Type != nil {
		currentType := string(*disk.Properties.Encryption.Type)
		if !r.isTypeAllowed(currentType, allowedTypes) {
			return fmt.Errorf("disk encryption type '%s' is not supported for %s migration", currentType, toSKU)
		}
	}

	return nil
}

func (r *ConfigurableDiskEncryptionTypeRule) Name() string {
	return "DiskEncryptionType"
}

func (r *ConfigurableDiskEncryptionTypeRule) getAllowedEncryptionTypes(config ValidationRuleConfig) ([]string, error) {
	if types, exists := config.Params["allowedTypes"]; exists {
		if typeList, ok := types.([]interface{}); ok {
			var result []string
			for _, encType := range typeList {
				if typeStr, ok := encType.(string); ok {
					result = append(result, typeStr)
				}
			}
			return result, nil
		}
	}

	// Default: exclude double encryption
	return []string{
		"EncryptionAtRestWithPlatformKey",
		"EncryptionAtRestWithCustomerKey",
	}, nil
}

func (r *ConfigurableDiskEncryptionTypeRule) isTypeAllowed(encType string, allowedTypes []string) bool {
	for _, allowedType := range allowedTypes {
		if strings.EqualFold(encType, allowedType) {
			return true
		}
	}
	return false
}

// ConfigurableLogicalSectorSizeRule validates logical sector size with configuration
type ConfigurableLogicalSectorSizeRule struct{}

func (r *ConfigurableLogicalSectorSizeRule) ValidateWithConfig(ctx context.Context, disk *armcompute.Disk, fromSKU, toSKU armcompute.DiskStorageAccountTypes, config ValidationRuleConfig, diskParams azureutils.ManagedDiskParameters) error {
	requiredSize := r.getRequiredSectorSize(config)

	if disk.Properties != nil && disk.Properties.CreationData != nil && disk.Properties.CreationData.LogicalSectorSize != nil {
		sectorSize := *disk.Properties.CreationData.LogicalSectorSize
		if sectorSize != int32(requiredSize) {
			return fmt.Errorf("logical sector size must be %d bytes for %s migration, current: %d", requiredSize, toSKU, sectorSize)
		}
	}

	return nil
}

func (r *ConfigurableLogicalSectorSizeRule) Name() string {
	return "LogicalSectorSize"
}

func (r *ConfigurableLogicalSectorSizeRule) getRequiredSectorSize(config ValidationRuleConfig) int {
	if size, exists := config.Params["requiredSize"]; exists {
		if sizeInt, ok := size.(float64); ok { // JSON numbers are float64
			return int(sizeInt)
		}
		if sizeInt, ok := size.(int); ok {
			return sizeInt
		}
	}
	// Default: 512 bytes for PremiumV2_LRS
	return 512
}

// ConfigurablePerfProfileRule validates performance profile with configuration
type ConfigurablePerfProfileRule struct{}

func (r *ConfigurablePerfProfileRule) ValidateWithConfig(ctx context.Context, disk *armcompute.Disk, fromSKU, toSKU armcompute.DiskStorageAccountTypes, config ValidationRuleConfig, diskParams azureutils.ManagedDiskParameters) error {
	allowedProfiles, err := r.getAllowedProfiles(config)
	if err != nil {
		return err
	}

	// This would need to check disk tags or other metadata for performance profile
	// For now, implement basic validation
	klog.V(4).Infof("Performance profile validation with allowed profiles: %v", allowedProfiles)

	return nil
}

func (r *ConfigurablePerfProfileRule) Name() string {
	return "PerfProfile"
}

func (r *ConfigurablePerfProfileRule) getAllowedProfiles(config ValidationRuleConfig) ([]string, error) {
	if profiles, exists := config.Params["allowedProfiles"]; exists {
		if profileList, ok := profiles.([]interface{}); ok {
			var result []string
			for _, profile := range profileList {
				if profileStr, ok := profile.(string); ok {
					result = append(result, profileStr)
				}
			}
			return result, nil
		}
	}

	// Default: allow empty and basic profiles
	return []string{"", "basic"}, nil
}
