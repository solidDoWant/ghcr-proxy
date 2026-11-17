//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// robot is a Harbor robot account.
type robot struct{ username, password string }

// tenant is a CI namespace with a mirror for each upstream.
type tenant struct {
	namespace string
	// robots are the pull credentials of the namespace's Harbor projects, by upstream key.
	robots map[string]robot
}

func newTenants() []*tenant {
	return []*tenant{{namespace: "ci-a"}, {namespace: "ci-b"}}
}

// applyProxyObjects makes the objects of the admin namespace: for each upstream
// its serving certificate, its CA and its UpstreamRegistry, and the credential
// for upstream A. The proxy's Secrets Role covers this namespace.
func (e *env) applyProxyObjects(ctx context.Context) error {
	tenantNames := make([]string, 0, len(e.tenants))
	for _, tenant := range e.tenants {
		tenantNames = append(tenantNames, tenant.namespace)
	}

	for _, fake := range e.upstreams {
		certPEM, keyPEM, err := fake.servingCA.issue(fake.host)
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
`, fake.objectName(), proxyNamespace, fake.host, fake.registryHost(), e.harborURL, strings.Join(tenantNames, ", "))
		if err := e.apply(ctx, manifest); err != nil {
			return err
		}
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

// setupTenantObjects creates a namespace's mirror Secrets, the Role that lets
// the proxy read exactly those Secrets, a RegistryMirror for each upstream and
// the policy. Only the namespace with the credential gets the push rule.
func (e *env) setupTenantObjects(ctx context.Context, tenant *tenant, withCredential bool) error {
	secretNames := []string{"harbor-ca"}

	if err := e.secret(ctx, tenant.namespace, "harbor-ca", "Opaque", map[string][]byte{"ca.crt": e.harborCA}); err != nil {
		return err
	}

	var objects, rules strings.Builder

	for _, fake := range e.upstreams {
		name := "robot-" + fake.key
		secretNames = append(secretNames, name)

		robot := tenant.robots[fake.key]
		if err := e.secret(ctx, tenant.namespace, name, "Opaque", map[string][]byte{"username": []byte(robot.username), "password": []byte(robot.password)}); err != nil {
			return err
		}

		fmt.Fprintf(&objects, `---
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: RegistryMirror
metadata: {name: mirror-%[1]s, namespace: %[2]s}
spec:
  upstreamRef: {namespace: %[3]s, name: %[4]s}
  url: %[5]s
  pathPrefix: %[6]s
  secretRef: {name: %[7]s}
  caSecretRef: {name: harbor-ca}
  fallbackToUpstream: false
`, fake.key, tenant.namespace, proxyNamespace, fake.objectName(), e.harborURL, harborProject(tenant.namespace, fake), name)

		fmt.Fprintf(&rules, `    - name: mirror-%[1]s
      mirrorRef: {name: mirror-%[1]s}
      repositories: ["mirrored/**"]
      actions: [pull]
`, fake.key)
	}

	if withCredential {
		fmt.Fprintf(&rules, `    - name: push
      credentialRef: {namespace: %[1]s, name: credential-%[2]s}
      repositories: ["soliddowant/app"]
      actions: [pull, push]
`, proxyNamespace, e.upstreams[0].key)
	}

	// The proxy reads these Secrets and no others in the namespace.
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata: {name: builder, namespace: %[1]s}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: registrygate-mirror-secrets, namespace: %[1]s}
rules:
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [%[2]s]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: registrygate-mirror-secrets, namespace: %[1]s}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: registrygate-mirror-secrets}
subjects:
  - {kind: ServiceAccount, name: registrygate, namespace: %[3]s}
%[4]s---
apiVersion: registrygate.soliddowant.dev/v1alpha1
kind: RegistryAccessPolicy
metadata: {name: builder, namespace: %[1]s}
spec:
  serviceAccounts: [builder]
  rules:
%[5]s`, tenant.namespace, strings.Join(secretNames, ", "), proxyNamespace, objects.String(), rules.String())

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
		hosts = append(hosts, fake.host)

		ip, err := fake.storageIP(ctx, e)
		if err != nil {
			return "", err
		}

		aliases = append(aliases, fmt.Sprintf("    - ip: %s\n      hostnames: [%s]", ip, fake.storageHost))
		caItems = append(caItems, fmt.Sprintf("          - {key: %[1]s.crt, path: %[1]s/ca.crt}", fake.host))
	}

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
		caData[fake.host+".crt"] = append(append([]byte{}, fake.servingCA.pem...), fake.fakeCA...)
	}

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

	if _, err := e.kubectl(ctx, "", "-n", tenant.namespace, "wait", "--for=condition=Ready", "pod/build", "--timeout=300s"); err != nil {
		return err
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

		time.Sleep(2 * time.Second)
	}
}

// docker runs a shell script in the Docker CLI container of a namespace's CI pod.
func (e *env) docker(ctx context.Context, namespace, script string) (string, error) {
	return e.kubectl(ctx, "", "-n", namespace, "exec", "build", "-c", "docker", "--", "sh", "-ec", script)
}
