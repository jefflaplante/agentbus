# Running in containers

The [Containerfile](../Containerfile) builds one image that holds the
`agentbus` binary: the broker, and the same CLI for managing it and for
running agents. It works with Podman, Docker and Kubernetes.

| In the image | |
|---|---|
| Entrypoint | `agentbus`; the default command is `serve` |
| User | `65532`, not root |
| `/config` | `broker.json`, `tokens/` and `pending/` (`AGENTBUS_CONFIG_DIR`) |
| `/data` | sessions, queued messages, the dedupe window and the audit log (`AGENTBUS_STATE_DIR`) |
| Ports | `1883` plaintext MQTT, `8883` MQTT over TLS; the config's `listen` decides which is open |

Keep `/config` and `/data` on volumes. Without them a restart loses every
agent, token and queued message.

## Get the image

Each release publishes an image for `linux/amd64` and `linux/arm64` to
the GitHub Container Registry, tagged with the release and, for the
newest one, `latest`:

```bash
podman pull ghcr.io/jefflaplante/agentbus:v0.1.0
podman tag ghcr.io/jefflaplante/agentbus:v0.1.0 agentbus:v0.1.0   # the short name used below
```

Or build it from a checkout:

```bash
podman build -f Containerfile --build-arg VERSION=v0.1.0 -t agentbus:v0.1.0 .
```

`docker build` takes the same arguments, and `make image` runs it for you
(`make image CONTAINER=docker`). `VERSION` is what `agentbus --version`
reports. Docker reads `.dockerignore`, not `.containerignore`; copy the
file if you want the smaller build context:

```bash
cp .containerignore .dockerignore
```

For more than one architecture:

```bash
podman build --platform linux/amd64,linux/arm64 --manifest agentbus:v0.1.0 \
    -f Containerfile --build-arg VERSION=v0.1.0 .
```

## Podman or Docker

Every command below works with `docker` in place of `podman`.

### Start a broker

A new config listens on `127.0.0.1` only, which nothing outside the
container can reach. So create the config, open the listener, then start
the broker:

```bash
podman volume create agentbus-config
podman volume create agentbus-data

# one-off agentbus commands against the volumes
abus() { podman run --rm -v agentbus-config:/config -v agentbus-data:/data agentbus:v0.1.0 "$@"; }
abus config init
abus config set listen 0.0.0.0:1883 allow_insecure true

podman run -d --name agentbus --restart unless-stopped \
    -p 127.0.0.1:1883:1883 \
    -v agentbus-config:/config -v agentbus-data:/data \
    agentbus:v0.1.0
```

`allow_insecure` is needed because the broker refuses a plaintext listener
beyond loopback. Here that's safe only because `-p 127.0.0.1:1883:1883`
publishes the port to this machine alone. To reach the broker from other
hosts, use [TLS](#tls) instead.

### Manage it

The CLI inside the container finds the config and the admin token by
itself:

```bash
podman exec -it agentbus agentbus console
podman exec agentbus agentbus config agent add worker --capability summarize
podman logs -f agentbus
```

A running broker picks up config changes within a second, so adding an
agent needs no restart. Settings marked `restart` in the config editor
(`listen`, TLS, `mcp_listen`, `join_listen`) need `podman restart agentbus`.

### Connect an agent

An agent needs the broker's URL, its ID and its token. Copy the token out
of the config volume, keeping it private:

```bash
(umask 077; podman exec agentbus cat /config/tokens/worker.token > worker.token)
agentbus --url tcp://127.0.0.1:1883 --agent worker --token-file worker.token \
    handle --capability summarize --exec ./summarize.sh
```

Or run the agent from the same image, on a network it shares with the
broker:

```bash
podman network create agentbus
podman network connect agentbus agentbus

podman run -d --name worker --network agentbus \
    -v ./worker.token:/run/agentbus/token:ro,Z \
    -v ./summarize.sh:/usr/local/bin/summarize.sh:ro,Z \
    -e AGENTBUS_URL=tcp://agentbus:1883 -e AGENTBUS_AGENT=worker \
    -e AGENTBUS_TOKEN_FILE=/run/agentbus/token \
    agentbus:v0.1.0 handle --capability summarize --exec summarize.sh
```

The token file must be readable by user `65532` in the container. The
`:Z` relabels the files for SELinux; leave it off with Docker on a host
without SELinux.

Instead of copying a token, a host can
[ask to join](CLI.md#letting-an-agent-ask-to-join): set `join_listen`,
publish its port, run `agentbus join` on the new host, and approve it with
`podman exec agentbus agentbus config join approve NAME`.

### TLS

Mount the certificate and key, point the config at them, and publish
8883:

```bash
abus config set listen 0.0.0.0:8883 tls_cert /tls/tls.crt tls_key /tls/tls.key allow_insecure false

podman rm -f agentbus
podman run -d --name agentbus --restart unless-stopped \
    -p 8883:8883 \
    -v agentbus-config:/config -v agentbus-data:/data \
    -v ./certs:/tls:ro,Z \
    agentbus:v0.1.0
```

`./certs` holds `tls.crt` and `tls.key`, readable by user `65532`. Agents
then connect to `ssl://HOST:8883`, where `HOST` is a name the certificate
covers. For a certificate from your own CA, give clients the CA with
`SSL_CERT_FILE`:

```bash
SSL_CERT_FILE=./certs/ca.crt agentbus --url ssl://bus.example.com:8883 --agent worker \
    --token-file worker.token agents
```

Commands run inside the container reach the broker as `localhost`, and
they verify the certificate like any other client. So include `localhost`
in the certificate's names, and for a certificate from your own CA point
them at it:

```bash
podman exec -it -e SSL_CERT_FILE=/tls/ca.crt agentbus agentbus console
```

`agentbus config ...` edits the file and needs neither.

### Upgrade

Build or pull the new image, then replace the container. The volumes keep
everything:

```bash
podman rm -f agentbus
podman run -d --name agentbus ... agentbus:NEW-VERSION
```

Agents reconnect by themselves, and messages queued for them while the
broker was down are still there (see [RETENTION.md](RETENTION.md)).

## Kubernetes

[examples/kubernetes/agentbus.yaml](../examples/kubernetes/agentbus.yaml)
runs the broker as a one-pod StatefulSet with a Service:

```bash
kubectl apply -f examples/kubernetes/agentbus.yaml
kubectl rollout status statefulset/agentbus
```

It pulls `ghcr.io/jefflaplante/agentbus:v0.1.0`. To use your own build or
a private registry, push the image there and change the manifest's two
`image:` lines:

```bash
podman tag agentbus:v0.1.0 REGISTRY/agentbus:v0.1.0
podman push REGISTRY/agentbus:v0.1.0
```

The pod's init container creates the config on first start and sets the
listener each time the pod starts. Everything else in the config is yours
and is left alone.

Run one replica. The broker keeps its state on its own disk, so a second
pod would be a second, separate bus, not a copy of the first.

### TLS

With no TLS Secret the broker listens in plaintext on `agentbus:1883`.
That is for trying it out inside a cluster you trust. For anything else,
create a `kubernetes.io/tls` Secret named `agentbus-tls` whose
certificate covers the names agents use (`agentbus`,
`agentbus.NAMESPACE.svc`, and any external name), then restart the pod:

```bash
kubectl create secret tls agentbus-tls --cert=tls.crt --key=tls.key
kubectl rollout restart statefulset/agentbus
```

The broker then listens with TLS on `agentbus:8883` and the plaintext
port closes. [cert-manager](https://cert-manager.io) can issue and renew
the Secret; a renewed certificate takes effect when the pod restarts.

### Manage it

```bash
kubectl exec -it agentbus-0 -- agentbus console
kubectl exec agentbus-0 -- agentbus config agent add worker --capability summarize
kubectl logs -f agentbus-0
```

Config changes apply to the running broker within a second. They are
saved on the `config` volume, so they outlive the pod.

With TLS on, `console` and the other commands that connect verify the
certificate. Name the Service and, for a certificate from your own CA,
the CA (a cert-manager Secret carries it as `ca.crt`; for a self-signed
certificate use `tls.crt`):

```bash
kubectl exec -it agentbus-0 -- env SSL_CERT_FILE=/tls/ca.crt \
    agentbus --url ssl://agentbus:8883 console
```

### Run an agent as a pod

Put the agent's token in a Secret and mount it:

```bash
kubectl create secret generic worker-token \
    --from-literal=token="$(kubectl exec agentbus-0 -- cat /config/tokens/worker.token)"
```

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker
spec:
  replicas: 1
  selector:
    matchLabels: {app.kubernetes.io/name: worker}
  template:
    metadata:
      labels: {app.kubernetes.io/name: worker}
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        fsGroup: 65532
      containers:
        - name: worker
          image: ghcr.io/jefflaplante/agentbus:v0.1.0
          args: ["handle", "--capability", "summarize", "--exec", "wc -w"]
          env:
            - {name: AGENTBUS_URL, value: "ssl://agentbus:8883"}   # tcp://agentbus:1883 without TLS
            - {name: AGENTBUS_AGENT, value: worker}
            - {name: AGENTBUS_TOKEN_FILE, value: /run/agentbus/token}
            # For a certificate from your own CA, mount the CA and add:
            # - {name: SSL_CERT_FILE, value: /run/agentbus-ca/ca.crt}
          volumeMounts:
            - {name: token, mountPath: /run/agentbus, readOnly: true}
      volumes:
        - name: token
          secret: {secretName: worker-token, defaultMode: 0440}
```

Give each replica its own agent ID. Two pods with one ID knock each other
off the bus, so a Deployment like this one stays at one replica; for
several workers on one queue, run several Deployments (or a StatefulSet
that takes the ID from the pod name), each with its own agent and token.

### Reach it from outside the cluster

MQTT is plain TCP, so an HTTP Ingress won't carry it. Use a
`LoadBalancer` Service, or a TCP route on your gateway, and only with TLS
on:

```bash
kubectl patch service agentbus -p '{"spec":{"type":"LoadBalancer"}}'
```

The MCP endpoint (`mcp_listen`) and the join endpoint (`join_listen`) are
HTTP. To use them, set each to an address like `0.0.0.0:8765`, add the
port to the container and the Service, and restart the pod. Both take the
broker's TLS certificate.

### Health and metrics

The manifest sets `metrics_listen` to `127.0.0.1:9090`, and the probes
call `/readyz` and `/healthz` there from inside the container. The
metrics endpoint has no authentication and names every agent, so it stays
on loopback. To scrape `/metrics` with Prometheus, add a sidecar that
proxies it with authentication, or set `metrics_listen` to `0.0.0.0:9090`
with `allow_insecure` and restrict the port with a NetworkPolicy.

### Upgrade and backup

Change the image tag and apply; the StatefulSet replaces the pod and the
volumes stay. To back up, snapshot both volumes: `config` holds the
agents and tokens, `data` the sessions and queued messages.
