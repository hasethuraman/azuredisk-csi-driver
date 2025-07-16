# Azure Disk SKU Migration Validation Framework

## Overview

The Azure Disk CSI Driver includes a comprehensive validation framework for migrating disks from Premium_LRS (PremiumV1) to PremiumV2_LRS (PremiumV2). This document provides detailed information about the validation framework, its design, implementation, and usage.

## Table of Contents

1. [Architecture](#architecture)
2. [Migration Prerequisites](#migration-prerequisites)
3. [Validation Framework Components](#validation-framework-components)
4. [Configuration](#configuration)
5. [Usage](#usage)
6. [Validation Rules](#validation-rules)
7. [Integration](#integration)
8. [Troubleshooting](#troubleshooting)
9. [Examples](#examples)
10. [Best Practices](#best-practices)

## Architecture

The SKU migration validation framework is integrated into the Azure Disk CSI Driver's `ControllerModifyVolume` method and provides comprehensive pre-migration validation to ensure successful disk SKU migrations.

### Key Components

1. **SKU Migration Validator** - Core validation engine with configurable rules
2. **Configuration Manager** - Runtime configuration management via ConfigMap
3. **Validation Rules** - Modular validation logic for specific migration requirements
4. **Integration Layer** - Integration with existing CSI controller operations

### Framework Flow

```
ControllerModifyVolume Request
         ↓
   Parameter Parsing (azureutils.ParseDiskParameters)
         ↓
   SKU Change Detection
         ↓
   Validation Framework
         ↓
   ┌─────────────────────────────┐
   │ SkuMigrationConfigManager   │
   │ - Load from ConfigMap       │
   │ - Auto-refresh capability   │
   │ - Default fallback          │
   └─────────────────────────────┘
         ↓
   ┌─────────────────────────────┐
   │ ConfigurableSKUMigration    │
   │ Validator                   │
   │ - Rule orchestration        │
   │ - Error aggregation         │
   └─────────────────────────────┘
         ↓
   Individual Validation Rules:
   • Region Support
   • Caching Mode
   • Enable Bursting
   • Disk Encryption Type
   • Logical Sector Size
   • Performance Profile
         ↓
   Validation Results
         ↓
   Proceed with Migration
   or Return Error
```

## Migration Prerequisites

### Supported Migration Paths

Currently supported migration:
- **Premium_LRS** → **PremiumV2_LRS**

### General Prerequisites

1. **Disk Requirements**:
   - Only Premium_LRS disks created in zones can be migrated
   - Disk size constraints apply based on migration performance tiers
   - Maximum 40 concurrent migrations per subscription per region

2. **Kubernetes Requirements**:
   - CSI Driver version ≥ v1.30.2
   - VolumeAttributesClass feature enabled (for VAC approach)
   - In-tree Azure Disk volumes must be migrated to CSI

3. **Configuration Requirements**:
   - Workload must be stopped during migration
   - Volume must be detached from VM during SKU change
   - Appropriate Azure permissions for disk operations

## Validation Framework Components

### 1. SkuMigrationConfigManager

The Configuration Manager handles runtime configuration via Kubernetes ConfigMap:

```go
type SkuMigrationConfigManager struct {
    kubeClient    kubernetes.Interface
    configMapName string
    namespace     string
    config        *MigrationConfig
    mutex         sync.RWMutex
    stopCh        chan struct{}
    refreshTicker *time.Ticker
}
```

**Features**:
- Runtime configuration updates without driver restart
- Automatic configuration refresh with configurable interval
- Thread-safe configuration access
- Fallback to default settings when ConfigMap is unavailable
- JSON-based configuration format

**Constructor**:
```go
func NewSkuMigrationConfigManager(kubeClient kubernetes.Interface, configMapName, namespace string) *SkuMigrationConfigManager
```

### 2. ConfigurableSKUMigrationValidator

The core validation engine that orchestrates validation rules:

```go
type ConfigurableSKUMigrationValidator struct {
    configManager *SkuMigrationConfigManager
    rules         map[string]ConfigurableValidationRule
}
```

**Features**:
- Modular validation rule system
- Runtime enable/disable of validation rules
- Configurable validation parameters per rule
- Comprehensive error reporting and aggregation
- Support for both static and runtime-configurable validation

**Constructors**:
```go
// With ConfigMap support
func NewConfigurableSKUMigrationValidator(configManager *SkuMigrationConfigManager) *ConfigurableSKUMigrationValidator

// With default configuration (no ConfigMap needed)
func NewSKUMigrationValidator() *ConfigurableSKUMigrationValidator
```

### 3. Validation Rules

Individual validation rules implement the `ConfigurableValidationRule` interface:

```go
type ConfigurableValidationRule interface {
    ValidateWithConfig(ctx context.Context, disk *armcompute.Disk, 
                      fromSKU, toSKU armcompute.DiskStorageAccountTypes, 
                      config ValidationRuleConfig,
                      diskParams azureutils.ManagedDiskParameters) error
    Name() string
}
```

**Key Change**: The validation interface now uses `azureutils.ManagedDiskParameters` instead of `map[string]string` for better type safety and consistency with the CSI driver's parameter handling.

### 4. ManagedDiskParameters Integration

The framework integrates with the CSI driver's parameter parsing system:

```go
type ManagedDiskParameters struct {
    AccountType             string
    CachingMode             v1.AzureDataDiskCachingMode
    DeviceSettings          map[string]string
    DiskAccessID            string
    DiskEncryptionSetID     string
    DiskEncryptionType      string
    DiskIOPSReadWrite       string
    DiskMBPSReadWrite       string
    DiskName                string
    EnableBursting          *bool
    PerformancePlus         *bool
    FsType                  string
    Location                string
    LogicalSectorSize       int
    MaxShares               int
    NetworkAccessPolicy     string
    PublicNetworkAccess     string
    PerfProfile             string
    SubscriptionID          string
    ResourceGroup           string
    Tags                    map[string]string
    UserAgent               string
    VolumeContext           map[string]string
    WriteAcceleratorEnabled string
    Zoned                   string
}
```

This provides type-safe access to all disk parameters and integrates seamlessly with the existing CSI driver architecture.

## Configuration

### ConfigMap Structure

The validation framework is configured via a Kubernetes ConfigMap with the following structure:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: azuredisk-sku-migration-config
  namespace: kube-system
data:
  config.json: |
    {
      "migrationPairs": {
        "Premium_LRS->PremiumV2_LRS": {
          "enabled": true,
          "validationRules": {
            "RegionSupport": {
              "enabled": true,
              "params": {
                "supportedRegions": [
                  "eastus", "westus2", "northeurope", "westeurope",
                  "eastasia", "southeastasia", "japaneast", "australiaeast",
                  "uksouth", "francecentral", "canadacentral", "brazilsouth",
                  "centralindia", "koreacentral"
                ]
              }
            },
            "CachingMode": {
              "enabled": true,
              "params": {
                "allowedModes": ["None"]
              }
            },
            "EnableBursting": {
              "enabled": true,
              "params": {
                "allowBursting": false
              }
            },
            "DiskEncryptionType": {
              "enabled": true,
              "params": {
                "allowedTypes": [
                  "EncryptionAtRestWithPlatformKey",
                  "EncryptionAtRestWithCustomerKey"
                ]
              }
            },
            "LogicalSectorSize": {
              "enabled": true,
              "params": {
                "requiredSize": 512
              }
            },
            "PerfProfile": {
              "enabled": true,
              "params": {
                "allowedProfiles": ["", "basic"]
              }
            }
          }
        }
      }
    }
```

### Configuration Parameters

#### Migration Pair Configuration

| Parameter | Type | Description | Default |
|-----------|------|-------------|---------|
| `enabled` | boolean | Enable/disable the migration pair | `true` |
| `validationRules` | object | Individual validation rule configurations | - |

#### Validation Rule Configuration

Each validation rule supports:

| Parameter | Type | Description |
|-----------|------|-------------|
| `enabled` | boolean | Enable/disable the specific validation |
| `params` | object | Rule-specific configuration parameters |

### Default Configuration

When no ConfigMap is provided, the framework uses built-in defaults:

```go
func getDefaultMigrationConfig() *MigrationConfig {
    return &MigrationConfig{
        MigrationPairs: map[string]*MigrationPairConfig{
            "Premium_LRS->PremiumV2_LRS": {
                Enabled: true,
                ValidationRules: map[string]*ValidationRuleConfig{
                    "RegionSupport": {
                        Enabled: true,
                        Params: map[string]interface{}{
                            "supportedRegions": []string{
                                "eastus", "westus2", "northeurope", "westeurope",
                                "eastasia", "southeastasia", "japaneast", "australiaeast",
                                "uksouth", "francecentral", "canadacentral", "brazilsouth",
                                "centralindia", "koreacentral",
                            },
                        },
                    },
                    // ... other default rules
                },
            },
        },
    }
}
```

## Usage

### 1. Deploy Configuration

Create the configuration ConfigMap:

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: azuredisk-sku-migration-config
  namespace: kube-system
data:
  config.json: |
    {
      "migrationPairs": {
        "Premium_LRS->PremiumV2_LRS": {
          "enabled": true,
          "validationRules": {
            "RegionSupport": {
              "enabled": true,
              "params": {
                "supportedRegions": ["eastus", "westus2", "northeurope"]
              }
            },
            "CachingMode": {
              "enabled": true,
              "params": {
                "allowedModes": ["None"]
              }
            },
            "EnableBursting": {
              "enabled": true,
              "params": {
                "allowBursting": false
              }
            },
            "DiskEncryptionType": {
              "enabled": true,
              "params": {
                "allowedTypes": [
                  "EncryptionAtRestWithPlatformKey",
                  "EncryptionAtRestWithCustomerKey"
                ]
              }
            },
            "LogicalSectorSize": {
              "enabled": true,
              "params": {
                "requiredSize": 512
              }
            },
            "PerfProfile": {
              "enabled": true,
              "params": {
                "allowedProfiles": ["", "basic"]
              }
            }
          }
        }
      }
    }
EOF
```

### 2. Initiate Migration

#### Using VolumeAttributeClass (VAC) Approach

1. Create VolumeAttributeClass:

```yaml
apiVersion: storage.k8s.io/v1alpha1
kind: VolumeAttributesClass
metadata:
  name: premium2-disk-class
driverName: disk.csi.azure.com
parameters:
  skuName: PremiumV2_LRS
  cachingMode: none
  enableBursting: "false"
  LogicalSectorSize: "512"
  DiskIOPSReadWrite: "3000"
  DiskMBpsReadWrite: "125"
```

2. Stop workload and wait for VolumeAttachment deletion

3. Patch PVC with VAC:

```bash
kubectl patch pvc <pvc-name> --patch '{"spec": {"volumeAttributesClassName": "premium2-disk-class"}}'
```

#### Using Direct Parameter Modification

The validation framework integrates with the CSI `ControllerModifyVolume` operation. Parameters are parsed using `azureutils.ParseDiskParameters()` and validated against the configured rules.

### 3. Monitor Migration

Track migration progress:

```bash
az disk show -n <disk-name> -g <resource-group> --query [completionPercent] -o tsv
```

### 4. Configuration Management

#### Enable Auto-Refresh

```go
configManager := NewSkuMigrationConfigManager(kubeClient, "azuredisk-sku-migration-config", "kube-system")
configManager.LoadConfig()
configManager.StartAutoRefresh(5 * time.Minute) // Refresh every 5 minutes
```

#### Stop Configuration Manager

```go
configManager.Stop()
```

## Validation Rules

### 1. Region Support Validation

**Purpose**: Ensures the disk is in a region that supports PremiumV2_LRS.

**Configuration**:
```json
"RegionSupport": {
  "enabled": true,
  "params": {
    "supportedRegions": ["eastus", "westus2", "northeurope"]
  }
}
```

**Validation Logic**:
- Checks if disk location is in the supported regions list
- Fails if region is not supported for PremiumV2_LRS
- Uses disk location from `armcompute.Disk.Location`

### 2. Caching Mode Validation

**Purpose**: Validates that caching mode is compatible with PremiumV2_LRS.

**Configuration**:
```json
"CachingMode": {
  "enabled": true,
  "params": {
    "allowedModes": ["None"]
  }
}
```

**Validation Logic**:
- PremiumV2_LRS only supports "None" caching mode
- Validates `diskParams.CachingMode` from parsed parameters
- Case-insensitive comparison

### 3. Enable Bursting Validation

**Purpose**: Ensures bursting is disabled for PremiumV2_LRS migration.

**Configuration**:
```json
"EnableBursting": {
  "enabled": true,
  "params": {
    "allowBursting": false
  }
}
```

**Validation Logic**:
- PremiumV2_LRS does not support bursting
- Validates `diskParams.EnableBursting` is false or nil
- Checks both current disk settings and new parameters

### 4. Disk Encryption Type Validation

**Purpose**: Validates encryption compatibility with PremiumV2_LRS.

**Configuration**:
```json
"DiskEncryptionType": {
  "enabled": true,
  "params": {
    "allowedTypes": [
      "EncryptionAtRestWithPlatformKey",
      "EncryptionAtRestWithCustomerKey"
    ]
  }
}
```

**Validation Logic**:
- Double encryption (`EncryptionAtRestWithPlatformAndCustomerKeys`) is not supported for migration
- Validates `diskParams.DiskEncryptionType` against allowed types
- Falls back to current disk encryption if not specified in parameters

### 5. Logical Sector Size Validation

**Purpose**: Ensures logical sector size is set to 512 bytes for PremiumV2_LRS.

**Configuration**:
```json
"LogicalSectorSize": {
  "enabled": true,
  "params": {
    "requiredSize": 512
  }
}
```

**Validation Logic**:
- PremiumV2_LRS requires 512-byte logical sector size
- Validates `diskParams.LogicalSectorSize` equals 512
- Checks current disk logical sector size if parameter not provided

### 6. Performance Profile Validation

**Purpose**: Validates performance profile compatibility.

**Configuration**:
```json
"PerfProfile": {
  "enabled": true,
  "params": {
    "allowedProfiles": ["", "basic"]
  }
}
```

**Validation Logic**:
- Advanced performance profiles may conflict with explicit IOPS/bandwidth settings
- Validates `diskParams.PerfProfile` against allowed profiles
- Empty string indicates no profile (default)

## Integration

### CSI Driver Integration

The validation framework integrates with the CSI driver's `ControllerModifyVolume` method:

```go
// In ControllerModifyVolume
func (d *Driver) ControllerModifyVolume(ctx context.Context, req *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
    // Parse volume attributes
    diskParams, err := azureutils.ParseDiskParameters(req.VolumeAttributesClass.Parameters)
    if err != nil {
        return nil, status.Errorf(codes.InvalidArgument, "failed to parse disk parameters: %v", err)
    }
    
    // Detect SKU change
    if diskParams.AccountType != currentSKU {
        // Initialize validator
        validator := NewConfigurableSKUMigrationValidator(d.skuMigrationConfigManager)
        
        // Validate migration
        if err := validator.ValidateMigration(ctx, disk, currentSKU, targetSKU, diskParams); err != nil {
            return nil, status.Errorf(codes.FailedPrecondition, "SKU migration validation failed: %v", err)
        }
    }
    
    // Proceed with migration...
}
```

### Error Handling

The framework provides detailed error messages for validation failures:

```go
// Example error output
"SKU migration validation failed: RegionSupport validation failed: disk region 'centralus' is not supported for PremiumV2_LRS migration. Supported regions: [eastus westus2 northeurope]"
```

Multiple validation errors are aggregated:

```go
// Multiple errors
"SKU migration validation failed: multiple validation errors: [RegionSupport validation failed: ..., CachingMode validation failed: ...]"
```

## Troubleshooting

### Common Validation Errors

#### Region Not Supported

**Error**: `RegionSupport validation failed: disk region 'centralus' is not supported for PremiumV2_LRS migration`

**Solution**: 
- Verify the disk is in a supported region
- Update the supported regions list in ConfigMap if needed
- Check Azure documentation for latest supported regions

#### Invalid Caching Mode

**Error**: `CachingMode validation failed: caching mode 'ReadWrite' is not supported for PremiumV2_LRS migration`

**Solution**:
- Update VolumeAttributeClass to set `cachingMode: none`
- Ensure the current disk caching mode is compatible

#### Bursting Enabled

**Error**: `EnableBursting validation failed: bursting must be disabled for PremiumV2_LRS migration`

**Solution**:
- Set `enableBursting: "false"` in VolumeAttributeClass parameters
- Verify current disk bursting configuration

#### Encryption Type Not Supported

**Error**: `DiskEncryptionType validation failed: disk encryption type 'EncryptionAtRestWithPlatformAndCustomerKeys' is not supported for migration`

**Solution**:
- Change encryption type to single encryption method
- Use either platform-managed or customer-managed keys, not both

#### Invalid Logical Sector Size

**Error**: `LogicalSectorSize validation failed: logical sector size must be 512 bytes for PremiumV2_LRS migration`

**Solution**:
- Ensure `LogicalSectorSize: "512"` in VolumeAttributeClass parameters
- Check current disk logical sector size

### Debug Information

Enable debug logging to get detailed validation information:

```bash
kubectl logs -n kube-system <csi-azuredisk-controller-pod> -c azuredisk
```

Look for log entries with:
- `Starting configurable SKU migration validation`
- `Running configurable validation rule`
- `Configuration loaded successfully`
- `Skipping disabled validation rule`

### Configuration Troubleshooting

#### ConfigMap Not Found

**Behavior**: Framework falls back to default configuration
**Log Message**: `ConfigMap azuredisk-sku-migration-config/kube-system not found, using default configuration`
**Solution**: Create the ConfigMap or verify namespace/name

#### Invalid Configuration JSON

**Error**: `failed to parse configuration: invalid character...`
**Solution**: 
- Validate JSON syntax
- Use `kubectl apply --dry-run=client` to check YAML syntax
- Test JSON parsing with online validators

#### Auto-Refresh Issues

**Log Message**: `Failed to refresh configuration: ...`
**Solution**:
- Check RBAC permissions for ConfigMap access
- Verify ConfigMap exists and has correct format
- Review controller logs for specific errors

## Examples

### Example 1: Production Configuration with All Validations

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: azuredisk-sku-migration-config
  namespace: kube-system
data:
  config.json: |
    {
      "migrationPairs": {
        "Premium_LRS->PremiumV2_LRS": {
          "enabled": true,
          "validationRules": {
            "RegionSupport": {
              "enabled": true,
              "params": {
                "supportedRegions": [
                  "eastus", "westus2", "northeurope", "westeurope",
                  "eastasia", "southeastasia", "japaneast", "australiaeast",
                  "uksouth", "francecentral", "canadacentral", "brazilsouth",
                  "centralindia", "koreacentral"
                ]
              }
            },
            "CachingMode": {
              "enabled": true,
              "params": {
                "allowedModes": ["None"]
              }
            },
            "EnableBursting": {
              "enabled": true,
              "params": {
                "allowBursting": false
              }
            },
            "DiskEncryptionType": {
              "enabled": true,
              "params": {
                "allowedTypes": [
                  "EncryptionAtRestWithPlatformKey",
                  "EncryptionAtRestWithCustomerKey"
                ]
              }
            },
            "LogicalSectorSize": {
              "enabled": true,
              "params": {
                "requiredSize": 512
              }
            },
            "PerfProfile": {
              "enabled": true,
              "params": {
                "allowedProfiles": ["", "basic"]
              }
            }
          }
        }
      }
    }

---
apiVersion: storage.k8s.io/v1alpha1
kind: VolumeAttributesClass
metadata:
  name: premium2-migration-class
driverName: disk.csi.azure.com
parameters:
  skuName: PremiumV2_LRS
  cachingMode: none
  enableBursting: "false"
  LogicalSectorSize: "512"
  DiskIOPSReadWrite: "3000"
  DiskMBpsReadWrite: "125"
```

### Example 2: Development Configuration with Relaxed Validations

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: azuredisk-sku-migration-config
  namespace: kube-system
data:
  config.json: |
    {
      "migrationPairs": {
        "Premium_LRS->PremiumV2_LRS": {
          "enabled": true,
          "validationRules": {
            "RegionSupport": {
              "enabled": false
            },
            "CachingMode": {
              "enabled": true,
              "params": {
                "allowedModes": ["None"]
              }
            },
            "EnableBursting": {
              "enabled": false
            },
            "DiskEncryptionType": {
              "enabled": false
            },
            "LogicalSectorSize": {
              "enabled": true,
              "params": {
                "requiredSize": 512
              }
            },
            "PerfProfile": {
              "enabled": false
            }
          }
        }
      }
    }
```

### Example 3: Migration with Custom Performance Parameters

```yaml
apiVersion: storage.k8s.io/v1alpha1
kind: VolumeAttributesClass
metadata:
  name: premium2-high-performance
driverName: disk.csi.azure.com
parameters:
  skuName: PremiumV2_LRS
  cachingMode: none
  enableBursting: "false"
  LogicalSectorSize: "512"
  DiskIOPSReadWrite: "10000"
  DiskMBpsReadWrite: "500"
  PerfProfile: ""
```

## Best Practices

### 1. Configuration Management

- **Version Control**: Store ConfigMap configurations in version control
- **Environment Specific**: Use different configurations for dev/staging/prod environments
- **Validation**: Test configuration changes in non-production environments first
- **Auto-Refresh**: Enable auto-refresh for production deployments with appropriate intervals

### 2. Migration Planning

- **Batch Processing**: Migrate disks in batches (max 40 per region per subscription)
- **Maintenance Windows**: Plan migrations during scheduled maintenance windows
- **Monitoring**: Set up monitoring for migration progress and completion
- **Testing**: Perform validation tests with representative workloads

### 3. Validation Strategy

- **Incremental Rollout**: Start with restrictive validation, gradually relax as needed
- **Regional Considerations**: Configure region support based on your deployment regions
- **Performance Requirements**: Align validation rules with application performance needs
- **Security Compliance**: Ensure encryption validation meets security requirements

### 4. Troubleshooting

- **Comprehensive Logging**: Enable detailed logging during migration planning and execution
- **Monitoring Integration**: Monitor Azure disk metrics during and after migration
- **Documentation**: Document custom configurations, exceptions, and lessons learned
- **Rollback Planning**: Have tested rollback procedures for failed migrations

### 5. Testing Framework

- **Unit Tests**: Use the provided test framework for validating custom configurations
- **Integration Tests**: Test end-to-end migration scenarios in staging environments
- **Performance Testing**: Validate performance characteristics after migration
- **Failure Scenarios**: Test validation failure scenarios and error handling

## Future Enhancements

The validation framework is designed to be extensible. Future enhancements may include:

1. **Additional Migration Paths**: Support for other SKU migrations (Standard_LRS → Premium_LRS, etc.)
2. **Custom Validation Rules**: Plugin system for customer-specific validation logic
3. **Automated Remediation**: Automatic fixing of certain validation failures
4. **Enhanced Monitoring**: Integration with Azure Monitor and Prometheus metrics
5. **Migration Orchestration**: Full automation of the migration process with retry logic
6. **Webhook Integration**: Admission controller integration for proactive validation
7. **Performance Profiling**: Automated performance impact assessment

## Support

For issues related to the SKU migration validation framework:

1. Check the [troubleshooting section](#troubleshooting)
2. Review CSI driver logs for detailed error information
3. Validate configuration JSON format and parameters
4. Test validation logic with unit tests
5. Consult Azure documentation for PremiumV2_LRS requirements
6. Open an issue in the [azuredisk-csi-driver repository](https://github.com/kubernetes-sigs/azuredisk-csi-driver)

## References

- [Azure Premium SSD v2 Documentation](https://docs.microsoft.com/en-us/azure/virtual-machines/disks-deploy-premium-v2)
- [Kubernetes VolumeAttributeClass](https://kubernetes.io/docs/concepts/storage/volume-attributes-classes/)
- [Azure Disk CSI Driver](https://github.com/kubernetes-sigs/azuredisk-csi-driver)
- [CSI Volume Modification](https://kubernetes-csi.github.io/docs/volume-modification.html)
- [Azure Disk Performance Tiers](https://docs.microsoft.com/en-us/azure/virtual-machines/disk-performance-tiers)