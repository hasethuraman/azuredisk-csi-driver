# SKU Migration Validation Examples

This directory contains example configurations for the SKU migration validation framework.

## Files

- `configmap.yaml` - Example ConfigMap configuration
- `volume-attribute-class.yaml` - Example VolumeAttributeClass for migration

## Usage

1. Deploy the ConfigMap:
```bash
kubectl apply -f configmap.yaml
```

2. Create the VolumeAttributeClass:

```bash
kubectl apply -f volume-attribute-class.yaml
```

3. Use the VolumeAttributeClass in your PVC modification.
Modify the ConfigMap to enable/disable specific validation rules or adjust parameters according to your requirements.