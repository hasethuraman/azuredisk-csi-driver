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
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// SkuMigration ConfigManager manages runtime configuration for SKU migration validation
type SkuMigrationConfigManager struct {
	kubeClient    kubernetes.Interface
	configMapName string
	namespace     string
	config        *MigrationConfig
	mutex         sync.RWMutex
	stopCh        chan struct{}
	refreshTicker *time.Ticker
}

// MigrationConfig represents the complete migration configuration
type MigrationConfig struct {
	MigrationPairs map[string]*MigrationPairConfig `json:"migrationPairs"`
}

// MigrationPairConfig represents configuration for a specific migration pair
type MigrationPairConfig struct {
	Enabled         bool                             `json:"enabled"`
	ValidationRules map[string]*ValidationRuleConfig `json:"validationRules"`
}

// ValidationRuleConfig represents configuration for a specific validation rule
type ValidationRuleConfig struct {
	Enabled bool                   `json:"enabled"`
	Params  map[string]interface{} `json:"params,omitempty"`
}

// NewSkuMigrationConfigManager creates a new configuration manager
func NewSkuMigrationConfigManager(kubeClient kubernetes.Interface, configMapName, namespace string) *SkuMigrationConfigManager {
	return &SkuMigrationConfigManager{
		kubeClient:    kubeClient,
		configMapName: configMapName,
		namespace:     namespace,
		config:        getDefaultMigrationConfig(),
		stopCh:        make(chan struct{}),
	}
}

// LoadConfig loads configuration from ConfigMap
func (cm *SkuMigrationConfigManager) LoadConfig() error {
	if cm.kubeClient == nil {
		klog.V(2).Infof("Kubernetes client not available, using default configuration")
		return nil
	}

	configMap, err := cm.kubeClient.CoreV1().ConfigMaps(cm.namespace).
		Get(context.TODO(), cm.configMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			klog.V(2).Infof("ConfigMap %s/%s not found, using default configuration", cm.namespace, cm.configMapName)
			cm.config = getDefaultMigrationConfig()
			return nil
		}
		return fmt.Errorf("failed to get ConfigMap %s/%s: %w", cm.namespace, cm.configMapName, err)
	}

	configData, exists := configMap.Data["config.json"]
	if !exists {
		return fmt.Errorf("config.json not found in ConfigMap %s/%s", cm.namespace, cm.configMapName)
	}

	var config MigrationConfig
	if err := json.Unmarshal([]byte(configData), &config); err != nil {
		return fmt.Errorf("failed to parse configuration: %w", err)
	}

	cm.mutex.Lock()
	cm.config = &config
	cm.mutex.Unlock()

	klog.V(2).Infof("Configuration loaded successfully from ConfigMap %s/%s", cm.namespace, cm.configMapName)
	return nil
}

// GetMigrationConfig returns configuration for a specific migration pair
func (cm *SkuMigrationConfigManager) GetMigrationConfig(fromSKU, toSKU string) (*MigrationPairConfig, error) {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	migrationKey := fmt.Sprintf("%s->%s", fromSKU, toSKU)
	config, exists := cm.config.MigrationPairs[migrationKey]
	if !exists {
		return nil, fmt.Errorf("migration configuration not found for %s", migrationKey)
	}

	return config, nil
}

// StartAutoRefresh starts automatic configuration refresh
func (cm *SkuMigrationConfigManager) StartAutoRefresh(interval time.Duration) {
	if cm.kubeClient == nil {
		klog.V(2).Infof("Kubernetes client not available, auto-refresh disabled")
		return
	}

	cm.refreshTicker = time.NewTicker(interval)

	go func() {
		for {
			select {
			case <-cm.refreshTicker.C:
				if err := cm.LoadConfig(); err != nil {
					klog.Warningf("Failed to refresh configuration: %v", err)
				} else {
					klog.V(4).Infof("Configuration refreshed successfully")
				}
			case <-cm.stopCh:
				return
			}
		}
	}()

	klog.V(2).Infof("Auto-refresh started with interval %v", interval)
}

// Stop stops the configuration manager
func (cm *SkuMigrationConfigManager) Stop() {
	if cm.refreshTicker != nil {
		cm.refreshTicker.Stop()
	}
	close(cm.stopCh)
	klog.V(2).Infof("Configuration manager stopped")
}

// getDefaultMigrationConfig returns the default configuration
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
							"allowedTypes": []string{
								"EncryptionAtRestWithPlatformKey",
								"EncryptionAtRestWithCustomerKey",
							},
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
