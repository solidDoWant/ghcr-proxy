//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// robot is a Harbor robot account.
type robot struct{ username, password string }

// tenant is a CI namespace with a mirror for each upstream.
type tenant struct {
	namespace string
	// pushRobot may push to the namespace's push project. Its Secret lives in the admin namespace only.
	pushRobot robot
}

func newTenants() []*tenant {
	return []*tenant{{namespace: "ci-a"}, {namespace: "ci-b"}}
}

// applyProxyObjects makes the objects of the admin namespace: for each upstream
// its serving certificate, its CA and its UpstreamRegistry, the credential
// for upstream A, and Harbor's UpstreamRegistry with a credential for the push
// project of each namespace. The proxy's Secrets Role covers this namespace.
func (e *env) applyProxyObjects(ctx context.Context) error {
	tenantNames := make([]string, 0, len(e.tenants))
	for _, tenant := range e.tenants {
		tenantNames = append(tenantNames, tenant.namespace)
	}

	for _, fake := range e.upstreams {
		certPEM, keyPEM, err := fake.servingCA.issue(fake.hosts...)
		if err != nil {
			return err
		}

		if err := e.secret(ctx, proxyNamespace, fake.objectName()+"-tls", "kubernetes.io/tls", map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM}); err != nil {
			return err
		}

		if err := e.secret(ctx, proxyNamespace, fake.objectName()+"-ca", "Opaque", map[string][]byte{"ca.crt": fake.fakeCA}); err != nil {
			return err
		}

		manifest := fmt.Sprintf(`
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: UpstreamRegistry
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  hosts: [%[3]s]
  url: https://%[4]s
  caSecretRef: {name: %[1]s-ca}
  tls:
    secretRef: {name: %[1]s-tls}
  anonymousPulls: Challenge
  allowedMirrorURLs: ["%[5]s/"]
  allowedNamespaces:
    names: [%[6]s]
`, fake.objectName(), proxyNamespace, strings.Join(fake.hosts, ", "), fake.registryHost(), e.harborURL, strings.Join(tenantNames, ", "))
		if err := e.apply(ctx, manifest); err != nil {
			return err
		}
	}

	if err := e.applyHarborUpstream(ctx, tenantNames); err != nil {
		return err
	}

	first := e.upstreams[0]

	if err := e.secret(ctx, proxyNamespace, "credential-"+first.key, "Opaque", map[string][]byte{"username": []byte("e2e"), "password": []byte(first.pat)}); err != nil {
		return err
	}

	// The credential may push to soliddowant/** on upstream A, and only the first tenant may use it.
	return e.apply(ctx, fmt.Sprintf(`
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: UpstreamCredential
metadata: {name: credential-%[1]s, namespace: %[2]s}
spec:
  upstreamRef: {name: %[3]s}
  secretRef: {name: credential-%[1]s}
  ceiling:
    - repositories: ["soliddowant/**"]
      actions: [pull, push]
  allowedNamespaces:
    names: [%[4]s]
`, first.key, proxyNamespace, first.objectName(), e.tenants[0].namespace))
}

// applyHarborUpstream makes the UpstreamRegistry for Harbor, which the proxy
// reaches through Harbor's core Service over plain HTTP, and for each namespace
// an UpstreamCredential for its push robot. The robot's Secret is in the admin
// namespace only. Harbor's externalURL host is one of the upstream's own
// hosts, so only the Service host is listed as an alias.
func (e *env) applyHarborUpstream(ctx context.Context, tenantNames []string) error {
	certPEM, keyPEM, err := e.harborProxyCA.issue(e.harborHostName)
	if err != nil {
		return err
	}

	if err := e.secret(ctx, proxyNamespace, "harbor-tls", "kubernetes.io/tls", map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM}); err != nil {
		return err
	}

	if err := e.apply(ctx, fmt.Sprintf(`
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: UpstreamRegistry
metadata: {name: harbor, namespace: %[1]s}
spec:
  hosts: [%[2]s]
  url: %[3]s
  aliasHosts: [harbor-core.harbor.svc]
  tls:
    secretRef: {name: harbor-tls}
  anonymousPulls: Challenge
  allowedNamespaces:
    names: [%[4]s]
`, proxyNamespace, e.harborHostName, e.harborURL, strings.Join(tenantNames, ", "))); err != nil {
		return err
	}

	for _, tenant := range e.tenants {
		name := "harbor-" + tenant.namespace

		if err := e.secret(ctx, proxyNamespace, name, "Opaque", map[string][]byte{"username": []byte(tenant.pushRobot.username), "password": []byte(tenant.pushRobot.password)}); err != nil {
			return err
		}

		if err := e.apply(ctx, fmt.Sprintf(`
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: UpstreamCredential
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  upstreamRef: {name: harbor}
  secretRef: {name: %[1]s}
  ceiling:
    - repositories: ["%[3]s/**"]
      actions: [pull, push, delete]
  allowedNamespaces:
    names: [%[4]s]
`, name, proxyNamespace, harborPushProject(tenant.namespace), tenant.namespace)); err != nil {
			return err
		}
	}

	return nil
}

// setupTenants gives each CI namespace its mirrors, its policy and a CI pod.
func (e *env) setupTenants(ctx context.Context) error {
	for index, tenant := range e.tenants {
		if err := e.setupTenantObjects(ctx, tenant, index == 0); err != nil {
			return err
		}
	}

	for _, tenant := range e.tenants {
		if err := e.setupCIPod(ctx, tenant); err != nil {
			return err
		}
	}

	return nil
}

// setupTenantObjects creates a namespace's RegistryMirror for each upstream
// and the policy. The namespace holds no Harbor credential: the mirrors read
// public projects, and the push robot is in the admin namespace. Only the
// namespace with the credential for upstream A gets that push rule.
func (e *env) setupTenantObjects(ctx context.Context, tenant *tenant, withCredential bool) error {
	var objects, rules strings.Builder

	for _, fake := range e.upstreams {
		fmt.Fprintf(&objects, `---
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: RegistryMirror
metadata: {name: mirror-%[1]s, namespace: %[2]s}
spec:
  upstreamRef: {namespace: %[3]s, name: %[4]s}
  url: %[5]s
  aliasHosts: [%[7]s, harbor-core.harbor.svc]
  pathPrefix: %[6]s
  fallbackToUpstream: false
`, fake.key, tenant.namespace, proxyNamespace, fake.objectName(), e.harborURL, harborProject(tenant.namespace, fake), strings.TrimPrefix(e.harborExternalURL, "https://"))

		fmt.Fprintf(&rules, `    - name: mirror-%[1]s
      mirrorRef: {name: mirror-%[1]s}
      repositories: ["%[2]s"]
      actions: [pull]
`, fake.key, fake.mirrorRepo)
	}

	fmt.Fprintf(&rules, `    - name: harbor-push
      credentialRef: {namespace: %[1]s, name: harbor-%[2]s}
      repositories: ["%[3]s/**"]
      actions: [pull, push]
`, proxyNamespace, tenant.namespace, harborPushProject(tenant.namespace))

	if withCredential {
		fmt.Fprintf(&rules, `    - name: push
      credentialRef: {namespace: %[1]s, name: credential-%[2]s}
      repositories: ["soliddowant/app"]
      actions: [pull, push]
`, proxyNamespace, e.upstreams[0].key)
	}

	manifest := fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata: {name: builder, namespace: %[1]s}
%[2]s---
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: RegistryAccessPolicy
metadata: {name: builder, namespace: %[1]s}
spec:
  serviceAccounts: [builder]
  rules:
%[3]s`, tenant.namespace, objects.String(), rules.String())

	if err := e.apply(ctx, manifest); err != nil {
		return err
	}

	// Every object must be ready before a test relies on it.
	for _, kind := range []string{"registrymirror", "registryaccesspolicy"} {
		if _, err := e.kubectl(ctx, "", "-n", tenant.namespace, "wait", "--for=condition=Ready", kind, "--all", "--timeout=120s"); err != nil {
			return err
		}
	}

	return nil
}

// ciPodManifest is the pod of the admin guide, set up for the hosts of both
// upstreams: a hostAliases entry for each host and each blob storage host,
// the CA for each host in the daemon container, the helper image volume, a
// projected token, and `docker-credential-registrygate configure` writes the
// Docker config.
func (e *env) ciPodManifest(ctx context.Context, tenant *tenant) (string, error) {
	var hosts, caItems []string

	aliases := []string{fmt.Sprintf("    - ip: %s\n      hostnames: [%s]", proxyClusterIP, "HOSTS")}

	for _, fake := range e.upstreams {
		hosts = append(hosts, fake.hosts...)

		ip, err := fake.storageIP(ctx, e)
		if err != nil {
			return "", err
		}

		aliases = append(aliases, fmt.Sprintf("    - ip: %s\n      hostnames: [%s]", ip, fake.storageHost))

		for _, host := range fake.hosts {
			caItems = append(caItems, fmt.Sprintf("          - {key: %[1]s.crt, path: %[1]s/ca.crt}", host))
		}
	}

	hosts = append(hosts, e.harborHostName)
	caItems = append(caItems, fmt.Sprintf("          - {key: %[1]s.crt, path: %[1]s/ca.crt}", e.harborHostName))

	aliases[0] = strings.Replace(aliases[0], "HOSTS", strings.Join(hosts, ", "), 1)

	return fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: build
  namespace: %[1]s
spec:
  restartPolicy: Always
  serviceAccountName: builder
  hostAliases:
%[2]s
  containers:
    - name: docker
      image: %[3]s
      command: [sh, -c]
      args:
        - |
          docker-credential-registrygate configure
          until docker info >/dev/null 2>&1; do sleep 1; done
          exec sleep infinity
      env:
        - {name: DOCKER_HOST, value: "tcp://localhost:2375"}
        - {name: REGISTRYGATE_HOSTS, value: "%[4]s"}
        - {name: PATH, value: "/opt/registrygate/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
      volumeMounts:
        - {name: registrygate-token, mountPath: /var/run/secrets/registrygate, readOnly: true}
        - {name: credential-helper, mountPath: /opt/registrygate/bin, readOnly: true}
    - name: dind
      image: %[5]s
      env:
        - {name: DOCKER_TLS_CERTDIR, value: ""}
      securityContext: {privileged: true}
      volumeMounts:
        - {name: registrygate-ca, mountPath: /etc/docker/certs.d, readOnly: true}
  volumes:
    - name: registrygate-token
      projected:
        sources:
          - serviceAccountToken: {audience: registrygate, expirationSeconds: 3600, path: token}
    - name: credential-helper
      image: {reference: %[6]s, pullPolicy: IfNotPresent}
    - name: registrygate-ca
      secret:
        secretName: registrygate-ca
        items:
%[7]s
`, tenant.namespace, strings.Join(aliases, "\n"), dockerCLIImage, strings.Join(hosts, ","), dockerDindImage, e.helperImage, strings.Join(caItems, "\n")), nil
}

func (e *env) setupCIPod(ctx context.Context, tenant *tenant) error {
	// The CA file of a host holds the proxy's CA for it and the CA of the fake behind it.
	caData := map[string][]byte{}

	for _, fake := range e.upstreams {
		for _, host := range fake.hosts {
			caData[host+".crt"] = append(append([]byte{}, fake.servingCA.pem...), fake.fakeCA...)
		}
	}

	caData[e.harborHostName+".crt"] = e.harborProxyCA.pem

	if err := e.secret(ctx, tenant.namespace, "registrygate-ca", "Opaque", caData); err != nil {
		return err
	}

	manifest, err := e.ciPodManifest(ctx, tenant)
	if err != nil {
		return err
	}

	if err := e.apply(ctx, manifest); err != nil {
		return err
	}

	stop := e.clearImageVolumeMountErrors(ctx, tenant.namespace)
	_, err = e.kubectl(ctx, "", "-n", tenant.namespace, "wait", "--for=condition=Ready", "pod/build", "--timeout=300s")

	stop()

	if err != nil {
		return fmt.Errorf("%w\n%s", err, e.podDiagnostics(ctx, tenant.namespace, "pod/build"))
	}

	deadline := time.Now().Add(2 * time.Minute)

	for {
		_, err := e.docker(ctx, tenant.namespace, "docker info")
		if err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("the Docker daemon in the CI pod of %s did not start: %w", tenant.namespace, err)
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// clearImageVolumeMountErrors works around a kernel rule that otherwise keeps
// a CI pod from ever starting. The node's containerd mounts an image volume as
// a volatile overlay, and the kernel refuses every volatile overlay mount with
// EIO ("Cannot mount volatile when upperdir has an unseen error") once the
// file system under it has recorded a write-back error that no one has seen
// yet. Two kind nodes share that file system in CI, and a single error blocks
// every later mount until a syncfs on it marks the error as seen. While the
// pod in namespace waits to start, this watches its events and, when the
// kubelet reports ImageVolumeMountFailed, runs syncfs on the node's
// containerd directory and prints the kernel's log, so that the kubelet's
// next retry mounts the volume. The returned function stops the watch.
func (e *env) clearImageVolumeMountErrors(ctx context.Context, namespace string) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		node := strings.TrimPrefix(e.kubeContext, "kind-") + "-control-plane"
		ticker := time.NewTicker(10 * time.Second)

		defer ticker.Stop()

		// Events stay for an hour: act only when the kubelet reports a new failure.
		var seen string

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			messages, err := e.kubectl(ctx, "", "-n", namespace, "get", "events",
				"--field-selector", "involvedObject.name=build,reason=Failed",
				"-o", `jsonpath={range .items[*]}{.metadata.resourceVersion} {.message}{"\n"}{end}`)
			if err != nil || messages == seen || !strings.Contains(messages, "ImageVolumeMountFailed") {
				continue
			}

			seen = messages

			// sync exits non-zero when it reports the error; reporting it is
			// what clears it.
			output, _ := exec.CommandContext(ctx, "docker", "exec", node, "sh", "-c",
				"sync -f /var/lib/containerd 2>&1; dmesg 2>&1 | grep -v 'Cannot mount volatile' | tail -n 40").CombinedOutput()
			fmt.Fprintf(os.Stderr, "e2e: an image volume mount failed in %s; ran syncfs on the node's containerd directory. Kernel log:\n%s\n", namespace, output)
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// podDiagnostics collects what is needed to tell why a pod did not become
// ready from the run log alone: the pod's description (status, conditions and
// events), the namespace events, the node's description and the container logs.
// It runs on a fresh context so it still works after the caller's deadline.
func (e *env) podDiagnostics(ctx context.Context, namespace, pod string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()

	var out strings.Builder

	for _, args := range [][]string{
		{"-n", namespace, "describe", pod},
		{"-n", namespace, "get", "events", "--sort-by=.lastTimestamp"},
		{"describe", "nodes"},
		{"-n", namespace, "logs", pod, "--all-containers", "--tail=50", "--prefix"},
	} {
		result, err := e.kubectl(ctx, "", args...)
		if err != nil {
			result = err.Error()
		}

		fmt.Fprintf(&out, "--- kubectl %s\n%s\n", strings.Join(args, " "), result)
	}

	// The image volume mount fails in the node's containerd: the node's disk and
	// kernel log, and the Docker host's disk, tell why.
	node := strings.TrimPrefix(e.kubeContext, "kind-") + "-control-plane"
	for _, args := range [][]string{
		{"exec", node, "sh", "-c", "df -h / /var/lib/containerd /run 2>&1; dmesg 2>&1 | grep -v 'Cannot mount volatile' | tail -n 40"},
		{"system", "df"},
	} {
		result, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			result = append(result, err.Error()...)
		}

		fmt.Fprintf(&out, "--- docker %s\n%s\n", strings.Join(args[:2], " "), result)
	}

	return out.String()
}

// docker runs a shell script in the Docker CLI container of a namespace's CI pod.
func (e *env) docker(ctx context.Context, namespace, script string) (string, error) {
	return e.kubectl(ctx, "", "-n", namespace, "exec", "build", "-c", "docker", "--", "sh", "-ec", script)
}
