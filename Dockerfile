# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.7 AS builder

WORKDIR /workspace
ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -a -o manager .

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
