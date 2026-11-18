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

- Every build pod runs in a kata VM (`runtimeClassName: kata`). The operator measured 32 vCPUs inside a build VM. A container's CPU request only places the pod on a node and sets its share of the CPU when containers compete. No container has a CPU limit, as the checklist requires.
- A kata pod's memory request must equal its limit. The VM gets the sum of the container limits plus 256 MiB.
- The project quota is 48 GiB of memory. At most 3 PipelineRuns of the repository run at once (`concurrency_limit: 3`). The rest wait in a queue. The owner set these values for this phase in infra-mk3#882.
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

A pull request run has two tasks that start at once, and a `finally` task. A `finally` task runs after all other tasks, also when one of them failed. A push run has the same shape without `test-guard`:

```
pull request:  ┌─ ci          (docker Task: clone, merge with master, skip rules, every stage in parallel)
               └─ test-guard  (step 1 reads the PR labels with the token, step 2 runs the guard with no token)
               finally: report (one check run for each stage, fails the run if a stage failed)
push:          └─ ci          (every stage, no skip rules, then the cache cleanup)
               finally: report
```

No step that runs code from the repository gets a token that can write to GitHub. Only `test-guard`'s first step and the `report` task get the PaC run token, and they run only the inline script in the PipelineRun file and a pinned image.

The PipelineRun files themselves are in `.tekton/`, and PaC reads them from the pull request. So this rule keeps the token away from the Makefile, the scripts and the Go dependencies that a stage runs. It does not stop a pull request that changes `.tekton/` itself. PaC runs pull requests only from senders it allows (section 3.5).

### 3.1 The `ci` task

`ci` is one docker Task pod. Its step clones the repository itself into an `emptyDir` workspace. The step image is the Docker CLI image, which has git, so no separate clone pod sits on the critical path.

The clone has the full history, as `ci.yml` uses `fetch-depth: 0`. The repository is about 1 MB. Three things need the history: the skip rules need the merge base with `master`, `make test` runs the test guard's own tests over the history, and the docs style report compares with `origin/master`.

- Pull request: the step fetches `master` and the PR head, checks out the head, and runs `git merge --no-ff origin/master`. The stages test this merge result, the same tree that `actions/checkout` tests on `pull_request` today. If the merge has a conflict, the step prints the conflicting files and the run fails. The pull request must then be rebased.
- Push: the step checks out the pushed commit.

Then the step runs `scripts/ci/stages.sh`. This script:

1. Decides which stages run. It uses the rules of `ci.yml` today. For a pull request, it applies them to the files that the PR changes: `git diff --name-only $(git merge-base HEAD^1 origin/master) HEAD^1`, where `HEAD^1` is the PR head. A push to master runs every stage.
2. Builds the CI image (section 4) and creates the Go cache volumes (section 5) if they do not exist.
3. Starts each selected stage as its own `docker run` of the CI image, all at the same time, with `--network host` so that the stages reach `dockerd` and kind's API server on `127.0.0.1`.
4. Writes each stage's output to its own file. When a stage finishes, the script prints that stage's whole log as one block with a header. It prints failed stages last.
5. Prints a table: each stage's result, time, peak memory and CPU time (section 6).
6. On a push run, cleans the Docker storage and checks its size (section 5).
7. Writes the Task result `stages`: for each stage, its name, result, time and a short failure report (section 3.4). Then it exits 0, also when a stage failed. The `report` task fails the run (section 3.3).

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
- Each docker Task pod has its own 3 GiB `dind` sidecar. One pod pays for that once.
- The stages share one Go build cache, so a package that lint, unit and e2e all compile is compiled once.

The cost is one log stream for all stages. The per-stage blocks and the final table keep it readable. The Tekton Dashboard shows the step log, and agents read it through the toolgate `tekton` MCP server.

The stage containers run inside the `dind` sidecar's cgroup, so the sidecar's memory limit is the memory of all stages together. The Task fixes it at 3 GiB. The PipelineRun raises it for this task with `taskRunSpecs[].sidecarSpecs[].computeResources` (section 6). If the cluster's Tekton does not allow that field, a `dind-memory` parameter on the docker Task is the fallback, which is an infra-mk3 change (section 8).

A stage container on the host network gets a copy of the `dind` container's `/etc/hosts`. This file has the entries that the `ci-registrygate-intercept` policy adds, so `ghcr.io`, `quay.io`, `mirror.gcr.io` and the other intercepted hosts resolve to registrygate in every stage. Every client in a stage then connects to registrygate: Docker, but also helm, crane and Go's HTTP client. For example, `TestAdminDocsGetToAPushAndAMirroredPull` runs `helm install` of a chart from `quay.io`.

So the stage containers trust the registrygate CA, the same setup that registrygate's users have:

- The `ci` step writes a CA bundle with the system CAs and the registrygate CA that the cluster gives `dockerd` (its `certs.d` directory). Each stage container mounts it and gets `SSL_CERT_FILE` set to it. The CA's name constraints limit it to the intercepted hosts.
- The `ci` step copies the credential helper, its token and the Docker configuration into the workspace, and each stage container mounts them at `~/.docker`. Docker, helm and crane read the credential helper from `~/.docker/config.json`.

Then `docker build`, `docker pull`, `helm` and `crane` in a stage go through registrygate like the step's own pulls. The kind node container is on Docker's `kind` network, not the host network. Docker writes its `/etc/hosts`, so the node's containerd still pulls directly from the internet, as it does on GitHub runners today.

### 3.2 The `test-guard` task

`test-guard` runs only for pull requests. It is one inline task with two steps. Both steps use the Python image from `mirror.gcr.io` (the full image, which has git, make and curl), pinned by digest. The steps share the task's workspace.

1. `labels` reads the PR's labels from the GitHub API with the PaC run token (`{{ git_auth_secret }}`), at run time, as `ci.yml` does. It writes them, comma-separated, to a file in the workspace. Only this step gets the Secret. It runs only its inline script and no code from the repository.
2. `guard` has no token. It clones the full history of the PR head and `master`, as the `ci` task does, and runs `make test-guard` with `TEST_GUARD_PR_LABELS` read from the file.

The PaC token has the installation permissions of the PaC GitHub App, which include writes to contents, issues and pull requests. The token is limited to this repository only. If code from the repository had the token, a pull request could add `test-removal-approved` to itself, or push a `v*` tag that starts `release.yml`. `ci.yml` gives this job a read-only token. The split keeps the PR's code away from a token with more rights than that.

After the owner adds `test-removal-approved`, a `/retest` comment starts a new run that reads the label. This run repeats all stages, about 8 minutes, where `ci.yml` re-ran only the guard job. This cost is accepted.

This task runs on every pull request, docs-only included. It finishes long before `ci`, so it adds no time.

### 3.3 The aggregate check

The PipelineRun is the aggregate. PaC reports it as one check. The check fails if any task fails, and a task skipped by a `when` expression counts as passed.

The `ci` step exits 0 when its stages ran, also when a stage failed. The `report` task reads the `ci` task's `stages` result. It fails if a stage failed, or if the result is missing because `ci` did not finish. A `finally` task that fails makes the PipelineRun fail. So the check fails when any stage fails, and it passes for a docs-only PR that runs only the docs stage and the test guard. The check links to the run in the Tekton Dashboard, where the failing stage's log block is.

The `ci` task does not fail on a failed stage, because a Tekton `finally` task cannot rely on the results of a failed task. The `stages` result must reach `report` in every case.

### 3.4 Logs, re-runs and the release gate for agents

The scheduler and the release-manager use GitHub Actions jobs today:

- The scheduler lists each failing job as a check run with a workflow run ID.
- It reads the job's failed log with `gh run view --log-failed` to tell a test assertion from an environment failure.
- It re-runs flaky jobs with `gh run rerun --failed`.
- The release-manager finds the `checks` run of a `master` commit with `gh run list`.

With Tekton, the PipelineRun is one check run with no workflow run. PaC copies no log lines into it (`error-log-snippet: false`). The toolgate `tekton` MCP server is read-only. It lists PipelineRuns and reads the logs of a pod that still exists. Agents can use it, but the scheduler is a Python script, not an agent. So the pipeline gives the scheduler what it needs through GitHub:

| Need | With Tekton |
|---|---|
| The failing stage | The `report` task creates one GitHub check run for each stage that ran, named `stage: <name>` (for example `stage: e2e`), with the stage's result and time. The check run links to the PipelineRun in the Tekton Dashboard. The scheduler lists failed stages from these check runs, as it lists failed jobs today. |
| The failure log | The check run's output text is the stage's failure report from the `stages` result: the lines from each `--- FAIL:` and `Error Trace:`, and the last lines of the stage log, cut to fit. The scheduler reads it with `gh api repos/solidDoWant/registrygate/check-runs/<id>` and classifies it with its current rules. |
| The full log | Agents read it with the `tekton` MCP server (`get_logs` on the `ci` TaskRun) while the pod exists. After that, they search VictoriaLogs by the pod name. |
| A re-run | The scheduler posts `/retest <PipelineRun name>` on the PR. It acts as the owner's account, and PaC accepts GitOps comments from the repository owner. The re-run repeats all stages, not only the failed one. |
| The release gate | The release-manager reads the check runs of the `master` commit (`gh api repos/solidDoWant/registrygate/commits/<sha>/check-runs`) and requires the push PipelineRun's check to have the conclusion `success`. |

The `report` task uses the PaC run token to create the check runs. It has the `checks: write` permission, because PaC needs it for its own check runs. Like `test-guard`'s first step, the `report` task runs only its inline script in a pinned image. If GitHub does not accept a stage check run, `report` prints the error and goes on. Only a failed stage or a missing result fails the run.

The `stages` result must fit Tekton's limit for Task results (4096 bytes by default). The `ci` script cuts each failure report so that the whole result fits.

The measurement issue changes the scheduler, `scripts/agents/wait-ci.sh` and `.claude/agents/release-manager.md` to these interfaces at the cutover (section 11). The pipeline issue makes sure that each row works on the cluster. If one needs a change in infra-mk3, it files a `needs-human` issue that blocks the measurement issue. Examples are a token without `checks: write`, `/retest` not accepted from the owner's account, or build pod logs missing from VictoriaLogs.

### 3.5 Pull requests from bots

PaC runs a pull request's PipelineRun only when it allows the sender: the owner, collaborators, organization members, or a login in the `OWNERS` file on the default branch. Renovate's pull requests come from the GitHub App `infra-mk3-renovate-operator`, whose webhook sender login is `infra-mk3-renovate-operator[bot]`. PaC does not allow this sender by default. Its pull requests then wait for `/ok-to-test` and get no check, so they cannot merge after the cutover. The webhook firewall already admits Renovate's events (infra-mk3 `coraza-waf.yaml`).

The pipeline issue adds an `OWNERS` file at the root that lists `infra-mk3-renovate-operator[bot]` as an approver. It makes sure that this works with a real Renovate pull request that runs on the cluster. If PaC does not run it, the pipeline issue files a `needs-human` infra-mk3 issue with the options and their trade-offs:

- A policy in the Repository resource's `settings.policy`.
- A `/ok-to-test` comment by the scheduler on each Renovate pull request.
- Other options that PaC gives at that time.

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
| Images for e2e that `dockerd` pulls or builds: the kind node image, the base images of the proxy, helper and fake registry images | Docker's image store and build cache | Image digests |

Pull request runs read all of these and cannot change what master reads. Each run gets its own copy-on-write clone of the newest golden snapshot, and the clone is deleted with the pod. The promoter snapshots only the volume of a successful push build on `master`. So only master runs update the shared caches. This needs no change in infra-mk3.

The `ci` TaskRun succeeds also when a stage fails (section 3.3). If the promoter reads the TaskRun's state and not the PipelineRun's, it can promote the volume of a master run with a failed stage. This is safe, because every cache is keyed on its inputs and the cleanup ran. The pipeline issue records which state the promoter reads.

Go's build cache is safe when several processes use it at once, so the parallel stages share one volume.

The snapshot must hold only caches. Stage containers run with `--rm`, and `make test-e2e` deletes the kind cluster it created. Before it exits, the `ci` script also removes any container and kind cluster that is left, so a master snapshot never holds a stopped cluster.

The images that the e2e tests install inside kind, such as Harbor, cert-manager and the `docker:*-dind` and `docker:*-cli` images, are not cached. The node's containerd pulls them, and the node container is deleted at the end of every run. So they come from the internet on every run, as on GitHub runners today. Section 6.1 names a fallback that caches them, if e2e on the cluster is too slow.

The volume is 30 GiB (infra-mk3#882). Docker keeps its build cache to 60% of that. The measured sizes after one run of every stage are in section 6.

### 5.1 Cleanup on push runs

The builder's garbage collection limits only the build cache, not Docker's image store. Each change to `flake.nix`, `flake.lock` or the Go version builds a new CI image of 3.4 GB, and the old one stays. There were 18 such commits from 2026-10-01 to 2026-11-18. Each run also leaves untagged e2e images. If the volume fills, master runs fail with "no space left on device". Then no new snapshot is promoted, and every later run starts from the same full snapshot.

So at the end of every push run, before the pod ends, `stages.sh` does these steps:

1. It removes untagged images (`docker image prune -f`).
2. It removes every CI image except the one with the current tag.
3. It cuts the build cache to a fixed budget (`docker builder prune -f --keep-storage`). The pipeline issue sets the budget from the measured steady-state size.
4. It reads the volume's use with `df`. If more than 80% is used, it prints `docker storage is <n>% full after cleanup` and the sizes from `docker system df`, and it fails the run as a failed `cleanup` stage.

Pull request runs do not clean up. Their volume is deleted with the pod, and only push runs on `master` shape what the promoter keeps. The pipeline issue records the volume's steady-state size after the cleanup.

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

The `test-guard` and `report` pods' steps get 256 MiB each. A pull request run then requests about 15 GiB, so 3 runs fit in the quota of 48 GiB. Stages run at the same time, so the memory peaks add up at most when the peaks happen together. The size is the sum of the cold peaks plus about 0.5 GiB for `dockerd`. Peaks do not all happen at once, and part of each peak is page cache, so a cold run does not starve a stage. A warm run uses up to 9.3 GiB, so the size is less than twice the measured peak.

The `ci` script records each stage's `memory.peak` and CPU time in its final table on every run. Issue work for the measurement replaces the numbers above with the cluster's own numbers, and it changes the sizes if they differ by more than 25%.

Stages that run at the same time compete for CPU inside one VM. On GitHub, each job had 4 vCPUs. A build VM has 32 vCPUs (section 2), so all stages together use less than the VM has (section 6 table). The CPU request of 8 places the pod on a node with free CPU and sets its share when pods on the node compete. It does not limit the VM.

### 6.1 Risks to check first

The pipeline issue checks these on the cluster before it builds the rest, because each one changes the design:

1. The e2e prototype below gives a total time that can beat GitHub.
2. The cluster's Tekton accepts `taskRunSpecs[].sidecarSpecs[].computeResources` for the `dind` memory.
3. A stage container on the host network resolves `quay.io` to registrygate, and `helm install` of a chart from `quay.io` succeeds with the mounted CA bundle and Docker configuration (section 3.1).
4. Go builds from the `emptyDir` workspace are not much slower than from the Docker volume. A kata `emptyDir` is shared into the VM over virtio-fs. If it is slow, the script copies the checkout into a Docker volume first.
5. The `report` task gets the `stages` result, can create check runs with the PaC token, and the scheduler's `/retest` starts a new run (section 3.4).

The first deliverable of the pipeline issue is a prototype with a fixed time limit. It is a draft PR with one PipelineRun of one docker Task that runs `make test-e2e` unchanged, in `ci-registrygate`. The prototype records the times of `kind create cluster`, the image builds and loads, `go test` and the whole run. The e2e stage on the cluster runs four levels deep: Docker inside kind, inside `dind`, inside a kata VM. kind's etcd and containerd write to a Ceph block volume with direct I/O. So the GitHub and VM numbers in this page do not predict it.

The goal is a pull request p50 below GitHub's (section 1). The e2e stage sets the run's time, so the limit is about 9 minutes for the whole PR run with e2e, about 8 minutes of it for the e2e stage. If the prototype is slower, the pipeline issue tries these fallbacks, cheapest first:

1. Pull the images that the e2e tests install inside kind with `dockerd`, where they are cached on the golden volume and go through the Harbor mirrors. Then put them into the node with `kind load`. This changes where the images come from, not what the tests test.
2. Put kind's etcd data on a tmpfs, with a `kubeadmConfigPatches` entry in the kind configuration.
3. Copy the checkout into a Docker volume (risk 4).

If e2e on the cluster still cannot beat GitHub's about 9 minutes, the pipeline issue posts the numbers on the phase 4 tracker and stops for the owner. Splitting the e2e tests is the owner's decision (section 7).

## 7. Wall clock

The expected time of a pull request run with a warm cache:

| Step | Time |
|---|---|
| PaC receives the event and creates the run | 5 s |
| Pod scheduled, volume cloned from the snapshot, kata VM boots, `dockerd` starts | 20 to 40 s |
| Fetch the full history of the PR head and `master`, merge `master` | 5 to 10 s |
| Build the CI image (cache hit) and create the cache volumes | 2 s |
| e2e: `kind create cluster` with the node image on the volume | 30 to 40 s |
| e2e: image builds (warm), load into kind | 10 s |
| e2e: `go test` | 380 s |
| Final table, pod ends, PaC reports | 5 s |
| Total | about 7.5 to 8 min |

Lint, unit, generate, integration and docs finish inside the e2e time. This is about 2 minutes faster than the GitHub Actions PR p50 of 9.6 minutes. On the agents' VM, the e2e stage alone took 483 s, kind and image builds included. A pull request without e2e (only Go tests changed) takes about 2.5 minutes, set by the integration and unit stages. A docs-only pull request takes about 1 minute.

The time in the queue is extra. At most 3 runs of the repository run at once, and pull request runs wait behind master runs.

These cases are slower:

- A cold cache: no golden snapshot yet, or a PR that changes `flake.lock` (plus 118 s for the image) or `go.sum` (plus the module downloads).
- A master push run before its snapshot exists: the next run still uses the previous snapshot. This is correct but uses older caches.

A further cut needs the owner. Run the e2e tests on two kind clusters in the same VM, split by test name. The longest test is 163 s, so two halves could take about 200 s instead of 377 s. The owner declined an e2e split for GitHub Actions. This design does not include it.

## 8. Changes needed in infra-mk3

Agents do not change infra-mk3. Each change below is its own `needs-human` + `area:ops` issue with the alternatives and trade-offs.

| Change | Issue | State |
|---|---|---|
| Onboard the repository | #176 | Done. |
| Project quota, concurrency and docker cache volume size | #182 | Done in infra-mk3#882: 48 GiB quota, `concurrency_limit: 3`, a 30 GiB docker cache volume. One run needs about 15 GiB. |
| vCPUs for CI kata VMs | #181 | Not needed. A build VM has 32 vCPUs. |

These changes are filed only if a check on the cluster shows that they are needed:

- A `dind-memory` parameter on the docker Task (below).
- What the agents' interface needs from infra-mk3 (section 3.4). Such an issue blocks the measurement issue.
- A PaC policy for Renovate's pull requests, if the `OWNERS` file is not enough (section 3.5).

The `dind` memory override (section 3.1) needs no change if the cluster's Tekton allows `sidecarSpecs`. Issue work checks that first, and files a request for a `dind-memory` Task parameter only if it does not.

## 9. Measuring

`scripts/ci/measure-ci.py` lists the commits on `master` and the commits of pull requests in a time window. For each commit it reads the check suites and check runs of one GitHub app (`--app`). The wall clock of a suite is from its creation to the end of its last check run. The queue is from its creation to the start of its first check run. It prints p50, p90 and maximum per event type, and the time of each check run.

- GitHub Actions: `--app github-actions --require e2e`. Each job is a check run, so the script also prints each job's time.
- Tekton: `--app` with the slug of the PaC GitHub App. The PipelineRun is one check run. PaC creates the check run when it creates the PipelineRun, so the queue time is in the wall clock. The `report` task creates the `stage: <name>` check runs with each stage's own start and end times (section 3.4), so the script prints the stage times too. These check runs end when the PipelineRun ends, so they do not change the wall clock.

The comparison for the checklist runs the script with the same options for both, over at least 10 runs of each event with e2e.

## 10. Release

The release stays on GitHub Actions (`release.yml`). It runs a few times per phase, so its time does not matter for the goal of this phase. It builds images for amd64 and arm64. In a kata VM, arm64 builds run under QEMU and are slow. It pushes to ghcr.io with the workflow's `GITHUB_TOKEN` and creates the GitHub release with it. On Tekton it needs the `ghcrPush` credential for the project and a GitHub token that can write releases inside a build pod. Both widen what a build can do, for no gain in time.

## 11. Cutover

The `checks` job of `ci.yml` stays the required check until Tekton is proven. Then:

1. Tekton runs next to `ci.yml` on every PR and master push, not required. The measurement issue collects at least 10 runs of each event type.
2. The owner adds the Tekton check to branch protection on `master`, next to `checks`. From now on both must pass.
3. One PR deletes `ci.yml` and changes the docs and scripts that name `checks`: `CLAUDE.md`, the agent playbooks, `scripts/agents/wait-ci.sh` and the scheduler's `required_check`. It also moves the scheduler's failed-job list, log reading and re-runs, and the release-manager's release gate, to the interfaces in section 3.4. Before it merges, the owner removes `checks` from branch protection. A PR that deletes `ci.yml` gets no `checks` run, so it cannot merge while `checks` is required.
4. `master` is protected at every step: by `checks` before step 2, by both in steps 2 and 3, and by the Tekton check after.

If Tekton is down, no PR can merge. The owner can add `checks` back by restoring `ci.yml` in one PR.

## 12. Issues

1. Done: onboard the repository (#176), sizes (#182). Not needed: vCPUs (#181).
2. The pipeline (#178): first the e2e prototype (section 6.1), then `.tekton/`, `scripts/ci/stages.sh`, `scripts/ci/changes.py`, the CI image build, the cleanup, the `report` task, the `OWNERS` file and the generated PipelineRuns.
3. Measure, make Tekton the required check, move the scheduler and the release-manager to the Tekton interfaces, retire `ci.yml` (#179). Blocked by #178, and by any `needs-human` issue from section 3.4.
