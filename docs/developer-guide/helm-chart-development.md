# Helm chart development

The chart lives under
`charts/argocd-clusterprofile-controller`.

## Release versions

The committed `version` and `appVersion` in:

```text
charts/argocd-clusterprofile-controller/Chart.yaml
```

are release-time placeholders, not published release versions.

Release artifacts are stamped from the Git tag. A tag such as `v0.1.0`
publishes:

- container images tagged `v0.1.0` and `0.1.0`
- a Helm chart packaged with `version: 0.1.0` and `appVersion: 0.1.0`

The release workflow also signs both the release image and OCI Helm chart with
cosign keyless signing.

Every push to `main` publishes a snapshot chart as `version: 0.0.0-main` with
`appVersion: main`. This snapshot is overwritten on later `main` pushes and is
intended for development use.

Install the latest `main` chart with:

```bash
helm install argocd-clusterprofile-controller \
  oci://ghcr.io/argoproj-labs/clusterprofile-integration-for-argocd/argocd-clusterprofile-controller \
  --version 0.0.0-main \
  --namespace argocd
```

## Local validation

To install the chart from a local checkout:

```bash
helm install argocd-clusterprofile-controller \
  ./charts/argocd-clusterprofile-controller \
  --namespace argocd \
  --set image.tag=main
```

Set `image.tag` to the container tag you want to run. The source chart's
`appVersion` is a placeholder and is not intended to resolve to a published
container image.

Run:

```bash
make helm-lint
make generate-values-schema
make validate-values-schema
make generate-helm-docs
```

`make helm-lint` runs `helm lint` against the chart.

`make generate-values-schema` regenerates `values.schema.json` from
`values.yaml` and `.schema.yaml`.

`make validate-values-schema` verifies that the generated schema is up to date
with the committed `values.schema.json`.

`make generate-helm-docs` regenerates the chart README from chart metadata and
`values.yaml` comments.

## End-to-end testing

`make e2e` creates one three-node kind hub and one spoke, installs two spread
controller replicas from the local checkout, and runs the full end-to-end test
suite against that shared hub. The install method defaults to
`helm`; set `E2E_INSTALL_METHOD=kustomize` to exercise the kustomize manifests
under `artifacts/manifests` instead. CI runs both methods via a matrix.
