## What and why

<!-- One or two sentences. Link the issue: Closes #123 -->

Closes #

## How it was tested

- [ ] `make test vuln`
- [ ] `make smoke layering security` against a running cluster
- [ ] `make postman` (if the storefront API changed; regenerate with `python3 postman/generate.py`)

## Checklist

- [ ] Branch name follows `type/<issue>-<slug>` and targets `develop`
- [ ] Layering still holds: BFF → business → domain only (AuthorizationPolicies updated if a call path changed)
- [ ] No secrets, tokens or `.certs/` content committed
- [ ] README / docs updated if behaviour or a `make` target changed
