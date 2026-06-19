# Releasing

Releases are driven by Git tags.

## Versioning

- Release tags must use `vX.Y.Z`.
- The container image is published to
  `ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd:vX.Y.Z`.
- The latest semver tag is also published as
  `ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd:latest`.
- The Helm chart is published as
  `oci://ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd/argocd-clusterprofile-controller`.
- The release workflow packages the chart with `version=X.Y.Z` and
  `appVersion=vX.Y.Z`.

## Process

1. Ensure CI is passing on `main`.
2. Create and push a release tag:

   ```bash
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

3. The release workflow builds and publishes the multi-arch container image and
   the OCI Helm chart to GHCR.

## Chart Development

Run these before opening a PR that changes the chart:

```bash
make validate-values-schema
make generate-helm-docs
```
