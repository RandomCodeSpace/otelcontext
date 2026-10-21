# Patched Go toolchain, pinned multi-platform index. No runtime OS packages.
# golang:1.26.10
FROM golang@sha256:5c3f3f28a273c9b61698b2d34f18375929790ab1f1d3835d032d21e2b31eae0c AS build
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal/ ./internal/
RUN go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /out/otelcontext . \
 && mkdir -p /out/data /out/tmp \
 && chmod 0700 /out/tmp

FROM scratch
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/RandomCodeSpace/otelcontext" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/otelcontext /otelcontext
COPY --from=build --chown=65532:65532 /out/data /var/lib/otelcontext
COPY --from=build --chown=65532:65532 /out/tmp /tmp
USER 65532:65532
WORKDIR /var/lib/otelcontext
EXPOSE 8080 4317
STOPSIGNAL SIGTERM
ENTRYPOINT ["/otelcontext"]
