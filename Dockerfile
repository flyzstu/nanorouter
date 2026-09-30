FROM golang:1.26-alpine AS build

WORKDIR /src

# 版本号由 CI 传入（tag 或分支名），本地构建默认 dev。
ARG VERSION=dev

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/nanorouter ./cmd/nanorouter
RUN mkdir -p /out/etc/nanorouter /out/var/lib/nanorouter \
    && cp config.example.yaml /out/etc/nanorouter/config.yaml

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build --chown=65532:65532 /out/nanorouter /usr/local/bin/nanorouter
COPY --from=build --chown=65532:65532 /out/etc/nanorouter/config.yaml /etc/nanorouter/config.yaml
COPY --from=build --chown=65532:65532 /out/var/lib/nanorouter /var/lib/nanorouter

USER 65532:65532
WORKDIR /var/lib/nanorouter

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/nanorouter"]
CMD ["--config", "/etc/nanorouter/config.yaml"]
