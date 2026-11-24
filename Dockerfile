# One Dockerfile for every service: docker build --build-arg SERVICE=services/domain/catalog .
# The builder is pinned by version and digest: the Go toolchain is part of every
# binary (net/http, crypto/tls), so it must not drift. Bump it together with the
# toolchain line in go.mod, and run `make vuln`.
FROM golang:1.26.11-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
WORKDIR /src
COPY go.mod go.sum ./
ENV GOTOOLCHAIN=local
RUN go mod download
COPY internal ./internal
COPY services ./services
ARG SERVICE
RUN test -n "$SERVICE" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app "./$SERVICE"

FROM scratch
COPY --from=build /out/app /app
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app"]
