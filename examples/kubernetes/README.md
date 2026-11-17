# Kubernetes manifest

[agentbus.yaml](agentbus.yaml) runs the broker as a one-pod StatefulSet
with a Service in front of it:

```bash
kubectl apply -f examples/kubernetes/agentbus.yaml
kubectl exec -it agentbus-0 -- agentbus console
```

The pod takes a few seconds to become ready; `kubectl rollout status
statefulset/agentbus` waits for it. The cluster needs a default
StorageClass for the two volumes.

The manifest pulls the image published with each release; change `image:`
(twice) to use your own. Building the image, TLS, adding agents and running
agents as pods are covered in
[Running in containers](../../docs/CONTAINERS.md#kubernetes).

| Piece | What it does |
|---|---|
| `Service/agentbus` | `agentbus:8883` (TLS) and `agentbus:1883` (plaintext) inside the namespace |
| `StatefulSet/agentbus` | one broker pod, non-root, read-only root filesystem |
| init container `configure` | creates the config on first start and sets the listener: TLS when the `agentbus-tls` Secret exists, plaintext otherwise |
| volume `config` | `broker.json`, `tokens/` and `pending/` |
| volume `data` | sessions, queued messages, the dedupe window and the audit log |

[bench.yaml](bench.yaml) is a separate pod and volume for running the
broker benchmarks on the cluster's own storage. See
[docs/BENCHMARKS.md](../../docs/BENCHMARKS.md#on-a-kubernetes-cluster).
