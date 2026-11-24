# Contributing

## Branches

```
main      ← releases only; every merge is tagged (v0.1.0, v0.2.0, …)
  ↑ release PR
develop   ← integration branch (default); CI must pass
  ↑ PR
feature/<issue>-<slug>    new capability            → Roadmap board
security/<issue>-<slug>   finding / hardening        → Security board
fix/<issue>-<slug>        bug fix                    → board of the area
chore/<issue>-<slug>      infra, CI, tooling, deps   → Platform & Ops board
docs/<issue>-<slug>       documentation only
hotfix/<issue>-<slug>     urgent fix cut from main, merged to main and develop
```

- Branch from `develop` (hotfixes from `main`), keep branches short-lived, one issue per branch.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/):
  `feat(bff): …`, `fix(inventory): …`, `sec(gateway): …`, `chore(ci): …`, `docs: …`.
- Open the PR into `develop` and put `Closes #<issue>` in the body so the issue and its board card
  close on merge.
- Release: open a PR `develop → main` titled `release: vX.Y.Z`, merge it with a merge commit, then
  tag `main` and close the milestone.

## Boards

| Project | What goes there | Extra fields |
|---|---|---|
| [meshshop · Roadmap](https://github.com/users/aquilacodes/projects/6) | features and epics | Layer, Priority, Size, Iteration |
| [meshshop · Security](https://github.com/users/aquilacodes/projects/7) | OWASP findings, hardening, accepted risks | Finding, Severity, OWASP, Risk |
| [meshshop · Platform & Ops](https://github.com/users/aquilacodes/projects/8) | cluster, mesh, identity, ELK/APM, CI, tooling | Area |

Issue forms add new issues to the right board. Every board has Backlog (table), Board,
Current iteration, Roadmap and My items views; Status is Todo → In progress → Done, and closing an
issue or merging its PR moves the card to Done.

## Labels

`type:*` (feature, bug, security, chore, docs, epic), `area:*` (bff, business, domain, platform-lib,
mesh, k8s, identity, observability, ci, api-tests), `sev:*` for security issues, and
`risk:accepted` for risks that are documented rather than fixed.

## Before opening a PR

```bash
make test vuln            # go vet + tests, govulncheck
make up                   # or the parts you touched
make gateway &            # then:
make smoke layering security postman
```

CI runs `go vet`, `go test -race`, govulncheck, `kustomize build` of every overlay, shellcheck and
gitleaks on every PR.
