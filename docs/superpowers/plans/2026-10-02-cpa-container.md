# Patched CPA Docker Image Implementation Plan

**Goal:** Build, validate and publish the approved patched CPA image from the
existing plugin repository, without deploying it.

**Authority:** `docs/superpowers/specs/2026-10-02-cpa-container-design.md`; the
user approved the image name, Linux/amd64 scope and GHCR publication.

## Files and independent lanes

- Writer: a dedicated Dockerfile, Docker ignore rules, `.github/workflows/cpa-image.yml`,
  small image-verification harness, README/image guide. No plugin behavior,
  dependency, registry, existing plugin workflow or CPA source changes.
- Research: public GHCR visibility/auth rules and repository Actions capability;
  read-only, independent from implementation.
- Review: read-only Docker/CI/verification diff before remote publication.
- Publishing: after review, push repository commits, dispatch workflow, inspect
  its terminal result, resolve digest and verify anonymous image pulling.

## Implementation steps

- [ ] Verify upstream SHA, patch application and build context exclusion.
- [ ] Add reproducible CGO-enabled Docker build with pinned CPA source,
  bookworm runtime and OCI provenance labels. Build source rather than copying
  the previously produced local binary.
- [ ] Add non-inference smoke/transport verification of the candidate image's
  actual binary and dynamic plugin loader. Synthetic plugin can be built with
  localhost usage endpoint; do not alter the production image's usage endpoint.
  Prefer a network-disabled verification container so no upstream is reachable.
- [ ] Assert 429/header known reset and omission for unknown reset for ordinary
  and initial stream paths. Keep healthy lower-priority/exhausted higher-priority
  fixture so scheduler, not broad fleet admission, produces the response.
- [ ] Add Actions test/build/publish stages with minimal permissions, Linux/amd64,
  default-branch/manual publishing restriction and no PR package writes.
- [ ] Document immutable image reference, retained plugin/config mounts and
  manual Dokploy image change. No automatic production update.
- [ ] Run local image build/test and workflow validation; save raw outputs in
  ignored `.superpowers/sdd/2026-10-02-cpa-container/`.
- [ ] Commit bounded Docker/CI/tests/docs and request independent review.
- [ ] Fix consequential review findings with scoped tests, then publish via
  Actions. Do not push the image before review acceptance.
- [ ] Verify workflow completion, image platform/source labels, immutable digest
  and anonymous pull. Report precise blockers if GHCR visibility needs user action.

## Verification budget

Do not repeat already-valid core CPA/plugin suites for Docker-only changes.
The writer establishes candidate-image native loading and wire behavior; reviewer
establishes build/CI security and correctness; parent reconciles publication and
public digest. Every success claim must distinguish local, CI and published-image
evidence. No real inference or authenticated usage checks are permitted.
