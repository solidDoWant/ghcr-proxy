# Phase 4 design: CI on Tekton

This page decides how registrygate's CI moves from GitHub Actions to the homelab's Tekton, which Pipelines-as-Code (PaC) drives. The goal is the shortest wall-clock time from a push to the required check. `SPEC.md` and the earlier phase designs still decide all product behaviour. This phase does not change the product.

The baseline, the goal and the checklist are in the phase 4 tracker and `docs/acceptance/phase-4.md`. The facts about the cluster come from infra-mk3 (`cluster/charts/ci/project`, `cluster/gitops/ci/tekton/`, `talos/patches/all/27-kata.yaml`) as of 2026-11-18.

## 1. Where the time goes today

`scripts/ci/measure-ci.py` reads the GitHub check runs of recent commits (section 9). It found these numbers for the newest 100 runs of `ci.yml` on 2026-11-16 and 2026-11-17, successful runs where e2e ran. These are the runs the tracker's baseline used, and the p50 and maximum agree with it within 0.1 minutes. The PR p90 is 11.6 minutes here and 10.8 in the tracker, with 31 runs here and 25 there: the selection of PR runs differs a little.

| Minutes | Pull request (n=31) | Push to master (n=33) |
|---|---|---|
| Wall clock, p50 / p90 / max | 9.6 / 11.6 / 13.5 | 10.2 / 16.2 / 18.8 |
| Queue before the first job, p50 / p90 | 0.1 / 0.1 | 0.0 / 6.2 |

Over all successful runs of the two days (54 PR runs, 50 push runs), the job times were these, setup included. The p50 of the PR wall clock was 8.9 minutes.

| Job, p50 minutes | PR | Push |
|---|---|---|
| e2e | 8.7 | 8.8 |
| integration | 3.5 | 3.6 |
| unit | 3.3 | 3.5 |
| lint | 2.6 | 2.6 |
| docs | 2.5 | 2.5 |
| generate | 2.2 | 2.3 |

Every job spends 2.0 to 2.7 minutes in `./.github/actions/nix-dev-shell` before it does any work: Nix install, the Nix store restore and the Go cache restore. The e2e job is the critical path. In one master run on 2026-11-17 it took these times after setup:

| e2e part | Time |
|---|---|
| `kind create cluster` | 41 s |
| Build the proxy and helper images, load them into kind | 10 s |
| `go test` of `test/e2e` (Harbor, cert-manager and the chart install inside the tests) | 377 s |

The longest single test is `TestAdminDocsGetToAPushAndAMirroredPull` at 163 s. So the e2e tests themselves, about 6.5 minutes, are the floor of this design. The design removes the setup cost and runs every other stage next to e2e.

## 2. What the cluster gives a project

The `ci/project` chart gives each repository a `ci-<name>` namespace. These facts constrain the design:

- Every build pod runs in a kata VM (`runtimeClassName: kata`). A kata VM boots with `default_vcpus = 1`, and kata adds vCPUs only for container CPU limits. The checklist forbids CPU limits. So with no change, every build VM has one vCPU. Section 8 asks the owner for a change.
- A kata pod's memory request must equal its limit. The VM gets the sum of the container limits plus 256 MiB.
- The project quota is 16 GiB of memory and 10 pods. At most 2 PipelineRuns of the repository run at once (`concurrency_limit`). The rest wait in a queue.
- The shared `docker` Task runs `dockerd` in a privileged sidecar (`dind`, 3 GiB, fixed in the Task). Its storage is a block volume. A Kyverno policy creates each build's volume as a copy-on-write clone of the project's newest "golden" snapshot. The promoter snapshots a volume only after a successful push build on the default branch, and then it takes the volume of the docker Task pod that finished last in that run. Pull request builds never qualify, because PaC sets the event label itself.
- The shared `git-clone` Task clones one revision at a depth you choose. Its image is the Docker CLI image, which contains git.
- PaC here does not fetch remote tasks or other `.tekton/` files. Each PipelineRun has its pipeline inline, and it uses Tasks only from `ci-tekton` through the cluster resolver.
- PaC posts one GitHub check run for each PipelineRun. It copies no log lines into the check (`error-log-snippet: false`). The check links to the Tekton Dashboard.

## 3. Pipeline shape

There are two PipelineRun files in `.tekton/`, one for each event:

| File | Trigger (`on-cel-expression`) | Cancel in progress |
|---|---|---|
| `pull-request.yaml` | `event == "pull_request" && target_branch == "master"` | Yes. A newer push to the PR cancels the older run. |
| `push.yaml` | `event == "push" && target_branch == "master"` | No. Every master run finishes, as with `ci.yml` today. |

The two files differ only in these annotations and in the `test-guard` task. They are generated from one template by `make generate` (`scripts/ci/tekton-generate.sh`), so `make check-generate` catches drift. The required check is the PaC check run of `pull-request.yaml`. Its name is the application name and the PipelineRun name. Issue work records the exact string.

Each PipelineRun has two tasks, and both start at once:

```
pull request:  ┌─ ci          (docker Task: clone, skip rules, every stage in parallel)
               └─ test-guard  (one Python step: clone, then the guard)
push:          └─ ci          (every stage, no skip rules)
```

### 3.1 The `ci` task

`ci` is one docker Task pod. Its step clones the commit itself into an `emptyDir` workspace. The step image is the Docker CLI image, which has git, so no separate clone pod sits on the critical path. Then the step runs `scripts/ci/stages.sh`. This script:

1. Decides which stages run. It uses the rules of `ci.yml` today, which it applies to `git diff --name-only` against the merge base with `master`. A push to master runs every stage.
2. Builds the CI image (section 4) and creates the Go cache volumes (section 5) if they do not exist.
3. Starts each selected stage as its own `docker run` of the CI image, all at the same time, with `--network host` so that the stages reach `dockerd` and kind's API server on `127.0.0.1`.
4. Writes each stage's output to its own file. When a stage finishes, the script prints that stage's whole log as one block with a header. It prints failed stages last.
5. Ends with a table: each stage's result, time, peak memory and CPU time (section 6). It exits non-zero if any stage failed.

The stages and their commands are the ones `ci.yml` runs today:

| Stage | Command | Runs when |
|---|---|---|
| lint | `make lint` | code changed |
| unit | `make test`, then `make build` | code changed |
| generate | `make check-generate` | code changed |
| integration | `make test-integration` | code changed |
| e2e | `make test-e2e` | code other than `*_test.go` changed, or `test/e2e/**` changed |
| docs | `make docs-check`, `./scripts/check-tracking-ids.py`, the style report (advisory) | Markdown or `scripts/docs/**` changed |
| scoreboard | `./scripts/acceptance-scoreboard_test.py` | `docs/acceptance/**` changed and code did not |

The path filters (code, e2e, acceptance, docs) are the ones in `ci.yml`. They move into `scripts/ci/changes.py`, which has its own tests. If no stage is selected, the script exits 0 at once. This is the case for a change only to `.claude/**`.

All stages run in one pod for three reasons:

- The promoter keeps the volume of only one docker Task pod per run. The Go caches and the CI image live on that volume, so all stages must write to the same one. With one pod per stage, only the stage that finished last keeps its cache.
- Each docker Task pod has its own 3 GiB `dind` sidecar. One pod pays for that once, inside a 16 GiB quota.
- The stages share one Go build cache, so a package that lint, unit and e2e all compile is compiled once.

The cost is one log stream for all stages. The per-stage blocks and the final table keep it readable. The Tekton Dashboard shows the step log, and agents read it through the toolgate `tekton` MCP server.

The stage containers run inside the `dind` sidecar's cgroup, so the sidecar's memory limit is the memory of all stages together. The Task fixes it at 3 GiB. The PipelineRun raises it for this task with `taskRunSpecs[].sidecarSpecs[].computeResources` (section 6). If the cluster's Tekton does not allow that field, a `dind-memory` parameter on the docker Task is the fallback, which is an infra-mk3 change (section 8).

Containers that `dockerd` starts do not see the pod's `/etc/hosts`, so the registrygate host entries do not apply to them. The `ci` step copies the credential helper, its token and the Docker configuration into the workspace, and the stage containers mount them. Then `docker build` and `docker pull` in the e2e stage go through registrygate like the step's own pulls. The kind node's containerd still pulls directly from the internet, as it does on GitHub runners today. Issue work measures whether `docker pull` of the e2e images (cached on the volume) and `kind load docker-image` is faster, and uses it if it is.

### 3.2 The `test-guard` task

`test-guard` runs only for pull requests. It is one inline task with one step in the Python image from `mirror.gcr.io` (the full image, which has git and make), pinned by digest. The step fetches the PR head and `master`, deepens the history until `git merge-base` succeeds, and runs `make test-guard`. It reads the PR labels from the GitHub API with the PaC run token (`{{ git_auth_secret }}`), at run time, as `ci.yml` does. The token goes only to this task, never to the `ci` pod, which runs the PR's code. After the owner adds `test-removal-approved`, a `/retest` comment starts a new run that reads the label.

This task runs on every pull request, docs-only included. It finishes long before `ci`, so it adds no time.

### 3.3 The aggregate check

The PipelineRun is the aggregate. PaC reports it as one check. The check fails if any task fails, and a task skipped by a `when` expression counts as passed. The `ci` task fails if any stage fails. So the check fails when any stage fails, and it passes for a docs-only PR that runs only the docs stage and the test guard. The check links to the run in the Tekton Dashboard, where the failing stage's log block is.

## 4. The toolchain: a CI image built from the flake

A stage needs the dev shell of `flake.nix`. Installing Nix and restoring the store in every stage costs 2 minutes today. Instead, the `ci` step builds one image with `docker build` and runs every stage in it:

```dockerfile
FROM ghcr.io/nixos/nix:2.35.6@sha256:b49cb33f3a7e5cf3933ad8774529de0e3807ec5cf54987aa7ccdfb49f2fc55ed
ENV NIX_CONFIG="experimental-features = nix-command flakes"
COPY flake.nix flake.lock go.mod /src/
RUN cd /src && git init -q && git add -A && git -c user.email=ci@ci -c user.name=ci commit -qm devshell \
 && nix develop --profile /nix/var/nix/profiles/devshell 'git+file:///src' -c true \
 && nix print-dev-env /nix/var/nix/profiles/devshell > /etc/devshell.sh \
 && nix-collect-garbage >/dev/null
```

Each stage runs `bash -c 'source /etc/devshell.sh && <command>'`. The image also gets the Docker CLI and buildx from nixpkgs, which the e2e stage needs and the dev shell does not have.

The build context is not the checkout. It holds only `flake.nix`, `flake.lock` and a `go.mod` reduced to its `module`, `go` and `toolchain` lines. The flake reads `go.mod` only for the Go version, and the reduced file changes only when the Go version changes. The flake refuses a path source, so the build commits the three files to a throwaway git repository first. Docker's layer cache, which is on the golden volume, is keyed on these files. So the image is rebuilt only when the flake, its lock or the Go version changes.

Measured on the agents' VM (8 vCPUs), 2026-11-18:

| Case | Time |
|---|---|
| Cold build (empty Docker cache, store from `cache.nixos.org`) | 118 s |
| Warm build (layer cache hit) | 1 s |
| Image size | 3.4 GB |

A PR that changes `flake.lock` pays the cold build once per run, until a master push with the change promotes a new golden volume.

These alternatives were rejected:

| Alternative | Why not |
|---|---|
| Nix in each stage, store on a cache volume | Needs a new cache volume mechanism in infra-mk3, and each stage still evaluates the flake (about 10 s). |
| An image pushed to ghcr.io from master builds | Needs `ghcrPush` and a policy for the project, and a PR that changes the flake still needs a local build. Nothing is faster than a 1 s cache hit. |
| A hand-written Dockerfile with the tools | A second toolchain definition that drifts from `flake.nix`. CI must use the agents' toolchain. |

## 5. Caches

All caches live on the docker Task's golden volume:

| Cache | Where | Key |
|---|---|---|
| Nix store (the dev shell) | The CI image, in Docker's image store and build cache | `flake.nix`, `flake.lock`, the Go version |
| Go module cache | Docker volume `gomod`, mounted at `GOMODCACHE` | None. Go checks module hashes against `go.sum`. |
| Go build cache | Docker volume `gobuild`, mounted at `GOCACHE` | None. Go keys each entry on its inputs and removes entries unused for 5 days. |
| golangci-lint cache | Docker volume `gobuild`, a subdirectory mounted at `GOLANGCI_LINT_CACHE` | None. golangci-lint keys entries on the package's files. |
| Images for e2e (kind node, base images, Harbor) | Docker's image store and build cache | Image digests |

Pull request runs read all of these and cannot change what master reads. Each run gets its own copy-on-write clone of the newest golden snapshot, and the clone is deleted with the pod. The promoter snapshots only the volume of a successful push build on `master`. So only successful master runs update the shared caches. This needs no change in infra-mk3.

Go's build cache is safe when several processes use it at once, so the parallel stages share one volume.

The snapshot must hold only caches. Stage containers run with `--rm`, and `make test-e2e` deletes the kind cluster it created. Before it exits, the `ci` script also removes any container and kind cluster that is left, so a master snapshot never holds a stopped cluster.

The default volume is 20 GiB. Docker keeps its build cache to 60% of that. The measured sizes after one run of every stage are in section 6. Section 8 asks for 30 GiB.

## 6. Resources

The `ci` pod's memory is the `dind` sidecar's limit, because the stage containers run inside it. The step needs little memory: it only starts containers and reads logs. No container has a CPU limit.

Measured on the agents' VM (8 vCPUs, 15 GiB), each stage alone in a container of the CI image, 2026-11-18. Peak memory is the container cgroup's `memory.peak`. CPU is `usage_usec` from `cpu.stat`, divided by the time.

| Stage | Cold Go cache: time, peak memory, average cores | Warm Go cache: time, peak memory, average cores |
|---|---|---|
| lint (`make lint`) | 68 s, 3.8 GiB, 5.8 | 9 s, 2.0 GiB, 4.3 |
| unit (`go test -race ./...`, `make build`) | 92 s, 4.1 GiB, 5.3 | 18 s, 1.7 GiB, 2.9 |
| integration (`make test-integration`) | 67 s, 3.4 GiB, 1.5 | 64 s, 3.5 GiB, 1.2 |
| generate (`make check-generate`) | not measured cold | 2 s, 0.2 GiB, 1.4 |
| docs (`make docs-check`) | not measured cold | 1 s, 0.04 GiB, 0.4 |
| e2e (`make test-e2e`) | 483 s (tests 386 s), 1.8 GiB | not measured warm |

`memory.peak` counts the page cache too, which the kernel can take back under pressure. So these peaks are an upper bound. The e2e peak is the kind node container's memory plus the resident memory of the test processes, sampled every 2 to 3 seconds on the host, because the kind node is not inside one container cgroup with the tests there.

With a cold cache, the peaks of all stages add up to 13.5 GiB. With a warm cache, they add up to 9.3 GiB. The Go caches after one cold run of every stage: build cache 3.8 GB, module cache 0.34 GB.

The e2e stage counts the kind node container, the image builds and the `go test` process together.

The `ci` pod's sizes:

| Container | Memory request = limit | CPU request |
|---|---|---|
| `dind` sidecar (all stages) | 14 GiB | 8 |
| `run` step | 256 MiB | 100m |

The `test-guard` pod's steps get 256 MiB each. A pull request run then requests about 14.5 GiB. Stages run at the same time, so the memory peaks add up at most when the peaks happen together. The size is the sum of the cold peaks plus about 0.5 GiB for `dockerd`. Peaks do not all happen at once, and part of each peak is page cache, so a cold run does not starve a stage. A warm run uses up to 9.3 GiB, so the size is less than twice the measured peak.

The `ci` script records each stage's `memory.peak` and CPU time in its final table on every run. Issue work for the measurement replaces the numbers above with the cluster's own numbers, and it changes the sizes if they differ by more than 25%.

Stages that run at the same time compete for CPU inside one VM. On GitHub, each job had 4 vCPUs. The VM needs at least 16 vCPUs so that e2e is not slowed by lint and the unit tests. Section 8 asks for this.

### 6.1 Risks to check first

The pipeline issue checks these on the cluster before it builds the rest, because each one changes the design:

1. `nproc` in a build pod prints 1 today, and the chosen setting from #181 raises it.
2. The cluster's Tekton accepts `taskRunSpecs[].sidecarSpecs[].computeResources` for the `dind` memory.
3. kind runs inside `dind` inside a kata VM, with the `ImageVolume` feature gate, and the e2e tests that run Docker inside kind pass there.
4. Go builds from the `emptyDir` workspace are not much slower than from the Docker volume. A kata `emptyDir` is shared into the VM over virtio-fs. If it is slow, the script copies the checkout into a Docker volume first.

## 7. Wall clock

The expected time of a pull request run with a warm cache:

| Step | Time |
|---|---|
| PaC receives the event and creates the run | 5 s |
| Pod scheduled, volume cloned from the snapshot, kata VM boots, `dockerd` starts | 20 to 40 s |
| Clone the commit (depth 1) | 3 s |
| Build the CI image (cache hit) and create the cache volumes | 2 s |
| e2e: `kind create cluster` with the node image on the volume | 30 to 40 s |
| e2e: image builds (warm), load into kind | 10 s |
| e2e: `go test` | 380 s |
| Final table, pod ends, PaC reports | 5 s |
| Total | about 7.5 to 8 min |

Lint, unit, generate, integration and docs finish inside the e2e time. This is about 2 minutes faster than the GitHub Actions PR p50 of 9.6 minutes. On the agents' VM, the e2e stage alone took 483 s, kind and image builds included. A pull request without e2e (only Go tests changed) takes about 2.5 minutes, set by the integration and unit stages. A docs-only pull request takes about 1 minute.

The time in the queue is extra. At most 2 runs of the repository run at once, and pull request runs wait behind master runs. Section 8 asks for 3.

These cases are slower:

- A cold cache: no golden snapshot yet, or a PR that changes `flake.lock` (plus 118 s for the image) or `go.sum` (plus the module downloads).
- A master push run before its snapshot exists: the next run still uses the previous snapshot. This is correct but uses older caches.

A further cut needs the owner. Run the e2e tests on two kind clusters in the same VM, split by test name. The longest test is 163 s, so two halves could take about 200 s instead of 377 s. The owner declined an e2e split for GitHub Actions. This design does not include it.

## 8. Changes needed in infra-mk3

Agents do not change infra-mk3. Each change below is its own `needs-human` + `area:ops` issue with the alternatives and trade-offs. The pipeline issue is blocked on them.

| Change | Issue | Why |
|---|---|---|
| vCPUs for CI kata VMs | #181 | With no CPU limit, a VM has 1 vCPU. |
| Project quota, concurrency and docker cache volume size | #182 | One run needs about 14.5 GiB. The quota of 16 GiB fits one run, and the volume must hold the CI image and the Go caches. |
| Onboard the repository | #176 | Already filed. |

The `dind` memory override (section 3.1) needs no change if the cluster's Tekton allows `sidecarSpecs`. Issue work checks that first, and files a request for a `dind-memory` Task parameter only if it does not.

## 9. Measuring

`scripts/ci/measure-ci.py` lists the commits on `master` and the commits of pull requests in a time window. For each commit it reads the check suites and check runs of one GitHub app (`--app`). The wall clock of a suite is from its creation to the end of its last check run. The queue is from its creation to the start of its first check run. It prints p50, p90 and maximum per event type, and the time of each check run.

- GitHub Actions: `--app github-actions --require e2e`. Each job is a check run, so the script also prints each job's time.
- Tekton: `--app` with the slug of the PaC GitHub App. The PipelineRun is one check run. PaC creates the check run when it creates the PipelineRun, so the queue time is in the wall clock. Stage times come from the `ci` task's final table.

The comparison for the checklist runs the script with the same options for both, over at least 10 runs of each event with e2e.

## 10. Release

The release stays on GitHub Actions (`release.yml`). It runs a few times per phase, so its time does not matter for the goal of this phase. It builds images for amd64 and arm64. In a one-vCPU or small kata VM, arm64 builds under QEMU are slow. It pushes to ghcr.io with the workflow's `GITHUB_TOKEN` and creates the GitHub release with it. On Tekton it needs the `ghcrPush` credential for the project and a GitHub token that can write releases inside a build pod. Both widen what a build can do, for no gain in time.

## 11. Cutover

The `checks` job of `ci.yml` stays the required check until Tekton is proven. Then:

1. Tekton runs next to `ci.yml` on every PR and master push, not required. The measurement issue collects at least 10 runs of each event type.
2. The owner adds the Tekton check to branch protection on `master`, next to `checks`. From now on both must pass.
3. One PR deletes `ci.yml` and changes the docs and scripts that name `checks`: `CLAUDE.md`, the agent playbooks, `scripts/agents/wait-ci.sh` and the scheduler's `required_check`. Before it merges, the owner removes `checks` from branch protection. A PR that deletes `ci.yml` gets no `checks` run, so it cannot merge while `checks` is required.
4. `master` is protected at every step: by `checks` before step 2, by both in steps 2 and 3, and by the Tekton check after.

If Tekton is down, no PR can merge. The owner can add `checks` back by restoring `ci.yml` in one PR.

## 12. Issues

1. Filed: onboard the repository (#176), vCPUs (#181), sizes (#182).
2. The pipeline (#178): `.tekton/`, `scripts/ci/stages.sh`, `scripts/ci/changes.py`, the CI image build, the generated PipelineRuns. Blocked by #176, #181 and #182.
3. Measure, make Tekton the required check, retire `ci.yml` (#179). Blocked by #178.
