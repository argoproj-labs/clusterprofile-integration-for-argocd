# Build the manager binary
FROM golang:1.26 AS builder

WORKDIR /workspace
# Copy argo-cd to satisfy the relative replace directive in go.mod (../argo-cd)
COPY argo-cd /workspace/argo-cd

# Copy our standalone controller into /workspace/clusterprofile-integration-for-argocd
WORKDIR /workspace/clusterprofile-integration-for-argocd
# Copy the Go Modules manifests
COPY clusterprofile-integration-for-argocd/go.mod go.mod
COPY clusterprofile-integration-for-argocd/go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY clusterprofile-integration-for-argocd/main.go main.go
COPY clusterprofile-integration-for-argocd/controller.go controller.go
COPY clusterprofile-integration-for-argocd/pkg/ pkg/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager main.go controller.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/clusterprofile-integration-for-argocd/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
