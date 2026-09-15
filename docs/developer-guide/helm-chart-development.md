# Helm chart development

The chart lives in [charts/argocd-clusterprofile-controller](../../charts/argocd-clusterprofile-controller).

## Release versions

The source [Chart.yaml](../../charts/argocd-clusterprofile-controller/Chart.yaml)
uses placeholder `version` and `appVersion` values. For source installs, set
`image.tag` to `main` or a locally built image tag.

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

After changing the chart, regenerate its files and validate it:

```bash
make generate-values-schema
make generate-helm-docs
make validate-values-schema
make helm-lint
```

## Generated files

| File | Source |
| --- | --- |
| `values.schema.json` | `values.yaml` schema annotations and `.schema.yaml` |
| `README.md` | `Chart.yaml`, `values.yaml` comments, and `README.md.gotmpl` |

Edit the source files when changing the schema or chart documentation. Generation
and validation targets are listed by `make help`.

## End-to-end testing

`make e2e` runs [hack/e2e-kind.sh](../../hack/e2e-kind.sh).
`E2E_INSTALL_METHOD` selects `helm` (the default) or `kustomize`.
