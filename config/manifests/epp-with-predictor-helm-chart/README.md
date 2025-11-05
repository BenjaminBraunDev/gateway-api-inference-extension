## 🔧 Quick Deploy Commands

### Using Helm Chart:

The Helm chart updates the EPP infrastructure with configurable prediction servers deployed as sidecars.

**Prerequisites:** These Helm charts assume you already have the EPP deployed with a working inference gateway. These charts just update the EPP deployment to include prediction sidecars and SLO-aware routing that incorporates predicted latencies for routing signals.

```bash
cd epp-with-predictor-helm-chart
helm install epp ./ --set predictionServers.count=10
```

### Cleanup:

```bash
helm uninstall epp
```

