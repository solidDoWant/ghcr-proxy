# ghcr-proxy documentation

For: anyone who wants to find the right page. You need to know nothing yet.

ghcr-proxy has four kinds of readers. Each page is for one of them. This page says who the readers are, which pages are theirs, and what each page covers.

## Who reads these docs

| Reader | What they already know | What they need | Pages |
|---|---|---|---|
| Chart authors (users) | Kubernetes YAML, Helm and Docker. | How to write a `GHCRAccessPolicy`, what its status means, and why a push is denied. | `docs/user/` |
| Admins | Kubernetes, Helm, Flux, cert-manager and Tekton. | How to install the proxy, the PAT, the certificate and its CA, the grants, how to set up a CI pod (host aliases, `certs.d`, the credential helper, the token), the security limits, and how to fix problems. | `docs/admin/` (the CRD and chart value references are generated) |
| Developers | Go, controller-runtime and the registry API. | The architecture, the packages, the fake GHCR, the test tiers and how to release. | `docs/dev/` |
| Agents | This repository. | Playbooks. Their pages do not follow the style rules for people. | `docs/agents/`, `.claude/`, `CLAUDE.md` |

The owner reads the acceptance checklists, the release notes and the status reports.

## Where each reader starts

- Chart authors: [`docs/user/`](user/README.md).
- Admins: [`docs/admin/`](admin/README.md).
- Developers: [`docs/dev/`](dev/README.md). [`SPEC.md`](../SPEC.md) has the full rules of the system.
- Agents: `CLAUDE.md`, then [`docs/agents/`](agents/planner.md).

## The tree

Every page under `docs/` is listed here. `make docs-check` fails when a page is missing from this list.

```
README.md                  The front door: what ghcr-proxy is and its status.
SPEC.md                    The rules of the system. For developers and agents.
CHANGELOG.md               The release notes.

docs/README.md             This page.

docs/user/
  README.md                The pages for chart authors.
  crd-reference.md         Every resource field (generated).

docs/admin/
  README.md                The pages for admins.
  chart-values.md          Every Helm chart value (generated).

docs/dev/
  README.md                What the developer pages will cover, and the commands.

docs/acceptance/
  phase-1.md               The owner's checklist for phase 1: push and pull through the proxy.
  phase-2.md               The owner's checklist for phase 2: delete, mirrors and operations.

docs/design/
  phase-1.md               The design for phase 1: types, config, request classification and the decision table.
  phase-2.md               The design for phase 2: delete, mirrors, metrics, the webhook and chart options.

docs/agents/               Instructions for the agents (not for people).
  planner.md               The planner's playbook.
```

## Rules for writing the pages

- Write one page for one reader. Put each fact on the page for the reader who acts on it, and link to it from other pages.
- Write in plain English. Agents load the `simple-english` skill before they write a page for people.
- Generate reference tables (CRD fields, chart values, metrics) from the code when they exist, so that they stay correct.
- `make docs-check` checks the links, this list and the generated pages. It does not count words or score the style.
