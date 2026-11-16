# Set up a CI pod

For: admins who set up the pods that run Docker builds. You know Kubernetes and Tekton. You already installed registrygate (see [Install registrygate](install.md)).

A CI pod pushes and pulls with the normal image name `ghcr.io/<owner>/<image>`. The pod sends the requests to the proxy because of the setup on this page. Docker finds its credential through the credential helper. The helper returns the ServiceAccount token of the pod, so the pod never sees the PAT.

## What each pod needs

- A `hostAliases` entry that maps `ghcr.io` to the fixed ClusterIP of the proxy Service (the value of `service.clusterIP`).
- A projected volume `registrygate-token`. It holds a ServiceAccount token with the audience `registrygate` and `expirationSeconds: 3600`, in the file `token`. Mount it at `/var/run/secrets/registrygate` in the container that runs the Docker CLI.
- An image volume from `ghcr.io/soliddowant/registrygate-credential-helper`, mounted at `/opt/registrygate/bin` in the same container. Add that directory to `PATH` of the container.
- The file `~/.docker/config.json` with `{"credHelpers":{"ghcr.io":"registrygate"}}`. The setup of the step writes it.
- The CA certificate at `/etc/docker/certs.d/ghcr.io/ca.crt` in the Docker daemon container (dind). The daemon makes the registry connections, so the CA belongs there and not in the CLI container.
- A `GHCRAccessPolicy` in the namespace of the pod that names the ServiceAccount of the pod. See [how to write a policy](../user/policies.md).

Pin the image digests of the helper and dind images in your own manifests.

## Pod example

This pod runs the Docker CLI against a dind sidecar. The Secret `registrygate-ca` in the pod namespace holds the CA certificate in the key `ca.crt`. Copy it from the `ca.crt` of the proxy certificate Secret (see [Install registrygate](install.md#make-the-tls-certificate)).

First make the namespace, the Secret with the CA certificate, the ServiceAccount and the policy. The policy lets the ServiceAccount push to `soliddowant/foo`. The grant in [Install registrygate](install.md#plan-the-grants) covers that repository for the namespace `ci-foo`.

<!-- e2e -->
```sh
kubectl create namespace ci-foo
kubectl -n registrygate get secret registrygate-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt
kubectl -n ci-foo create secret generic registrygate-ca --from-file=ca.crt=./ca.crt
kubectl -n ci-foo create serviceaccount builder
kubectl apply -f - <<'EOF'
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: GHCRAccessPolicy
metadata:
  name: builder
  namespace: ci-foo
spec:
  serviceAccounts: [builder]
  rules:
    - repositories: [soliddowant/foo]
      actions: [pull, push]
EOF
```

Set these variables to the images that you use. The pod below reads them. In your own manifests, write the images in the manifest:

- `DOCKER_CLI_IMAGE`: an image with the Docker CLI.
- `DOCKER_DIND_IMAGE`: an image with the Docker daemon.
- `HELPER_IMAGE`: `ghcr.io/soliddowant/registrygate-credential-helper:<version>`, where `<version>` is a release of the proxy.

The pod waits for the daemon, builds a small image and pushes it.

<!-- e2e -->
```sh
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: build
  namespace: ci-foo
spec:
  restartPolicy: Never
  serviceAccountName: builder
  hostAliases:
    - ip: 10.96.0.50          # service.clusterIP of the proxy
      hostnames: [ghcr.io]
  containers:
    - name: docker
      image: ${DOCKER_CLI_IMAGE}
      command: [sh, -c]
      args:
        - |
          mkdir -p ~/.docker
          echo '{"credHelpers":{"ghcr.io":"registrygate"}}' > ~/.docker/config.json
          until docker info >/dev/null 2>&1; do sleep 1; done
          printf 'FROM scratch\nLABEL built-by=ci\n' | docker build -t ghcr.io/soliddowant/foo:latest -
          docker push ghcr.io/soliddowant/foo:latest
      env:
        - name: DOCKER_HOST
          value: tcp://localhost:2375
        - name: PATH
          value: /opt/registrygate/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
      volumeMounts:
        - name: registrygate-token
          mountPath: /var/run/secrets/registrygate
          readOnly: true
        - name: credential-helper
          mountPath: /opt/registrygate/bin
          readOnly: true
    - name: dind
      image: ${DOCKER_DIND_IMAGE}
      env:
        - name: DOCKER_TLS_CERTDIR
          value: ""
      securityContext:
        privileged: true
      volumeMounts:
        - name: registrygate-ca
          mountPath: /etc/docker/certs.d/ghcr.io
          readOnly: true
  volumes:
    - name: registrygate-token
      projected:
        sources:
          - serviceAccountToken:
              audience: registrygate
              expirationSeconds: 3600
              path: token
    - name: credential-helper
      image:
        reference: ${HELPER_IMAGE}
        pullPolicy: IfNotPresent
    - name: registrygate-ca
      secret:
        secretName: registrygate-ca
EOF
```

The push worked when the `docker` container ends with exit code 0:

<!-- e2e -->
```sh
kubectl -n ci-foo wait --for=jsonpath='{.status.containerStatuses[?(@.name=="docker")].state.terminated.reason}'=Completed pod/build --timeout=600s
```

The helper image holds one file, `/docker-credential-registrygate`. Docker finds the helper by the name `docker-credential-registrygate` on `PATH`, and the config entry `registrygate` selects it.

The helper reads the token file on every call, so token rotation by the kubelet needs no action. To use another path, set `REGISTRYGATE_TOKEN_FILE`.

The Docker daemon runs in a privileged container here. Run it in a sandbox, for example a kata VM, as your own setup requires.

## Tekton Task example

A Tekton Task can use the same parts. The `serviceAccountName` of the TaskRun is the ServiceAccount that the policy names. The `podTemplate` of the TaskRun sets `hostAliases`.

```yaml
apiVersion: tekton.dev/v1
kind: Task
metadata:
  name: build-push
  namespace: ci-foo
spec:
  params:
    - name: image
      type: string
  workspaces:
    - name: source
  volumes:
    - name: registrygate-token
      projected:
        sources:
          - serviceAccountToken:
              audience: registrygate
              expirationSeconds: 3600
              path: token
    - name: credential-helper
      image:
        reference: ghcr.io/soliddowant/registrygate-credential-helper:<version>
        pullPolicy: IfNotPresent
    - name: registrygate-ca
      secret:
        secretName: registrygate-ca
  sidecars:
    - name: dind
      image: quay.io/example/docker-dind@sha256:...
      env:
        - name: DOCKER_TLS_CERTDIR
          value: ""
      securityContext:
        privileged: true
      volumeMounts:
        - name: registrygate-ca
          mountPath: /etc/docker/certs.d/ghcr.io
          readOnly: true
  steps:
    - name: build-push
      image: quay.io/example/docker-cli@sha256:...
      workingDir: $(workspaces.source.path)
      env:
        - name: DOCKER_HOST
          value: tcp://localhost:2375
        - name: PATH
          value: /opt/registrygate/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
      volumeMounts:
        - name: registrygate-token
          mountPath: /var/run/secrets/registrygate
          readOnly: true
        - name: credential-helper
          mountPath: /opt/registrygate/bin
          readOnly: true
      script: |
        mkdir -p ~/.docker
        echo '{"credHelpers":{"ghcr.io":"registrygate"}}' > ~/.docker/config.json
        docker build -t $(params.image) .
        docker push $(params.image)
---
apiVersion: tekton.dev/v1
kind: TaskRun
metadata:
  generateName: build-push-
  namespace: ci-foo
spec:
  taskRef:
    name: build-push
  serviceAccountName: builder
  params:
    - name: image
      value: ghcr.io/soliddowant/foo:latest
  podTemplate:
    hostAliases:
      - ip: 10.96.0.50
        hostnames: [ghcr.io]
  workspaces:
    - name: source
      emptyDir: {}
```

Check your Tekton version for support of `volumes` with image sources and of `podTemplate.hostAliases`.

## Check the setup

1. Make sure that a `GHCRAccessPolicy` for the ServiceAccount exists and has `Ready` set to `True`. See [what the status means](../user/policy-status.md).
2. Run `docker push` in the pod.
3. If the push fails, find the request in the [audit log](install.md#audit-log) and read its `decision`.

Common causes:

- `x509: certificate signed by unknown authority`: The CA is not at `/etc/docker/certs.d/ghcr.io/ca.crt` in the dind container.
- `401` from the proxy: The token file is missing, or the audience is not `registrygate`.
- `403 DENIED`: No policy and grant allow the push. See [why a push is denied](../user/policies.md#why-a-push-is-denied).
- `503`: The proxy is not ready or cannot review tokens. Try again.
