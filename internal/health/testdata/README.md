# Health test fixtures

Files in this directory are YAML test data used by `internal/health` tests.

- All files except the `patrol-application-*.yaml` fixtures are copied verbatim from the
  Argo CD project (https://github.com/argoproj/argo-cd), tag `v3.5.5`, commit
  `a5038b0e2bbac1715754380ae0084df8e3d5b48c`:
  - `gitops-engine/pkg/health/testdata/*.yaml` (same file names), and
  - `gitops-engine/pkg/utils/kube/testdata/nginx.yaml`, stored here as `deployment-nginx.yaml`.

  They are licensed under the Apache License, Version 2.0 (see the repository `LICENSE` and
  `NOTICE`). They are data only; no upstream code is used.
- `patrol-application-*.yaml` were written for Patrol CD.
