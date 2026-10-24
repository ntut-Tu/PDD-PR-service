# Build context: services/   (docker build -f images/git-gateway/Dockerfile -t pdd/git-gateway services)
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/git-gateway ./cmd/git-gateway \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
# A new named volume copies the ownership of this directory, so the nonroot user can write the database.
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/git-gateway /usr/local/bin/git-gateway
VOLUME /data
USER nonroot:nonroot
EXPOSE 8080
# The deployment mounts the merged configuration here (see cmd/git-gateway/README.md).
ENTRYPOINT ["/usr/local/bin/git-gateway"]
CMD ["--config", "/etc/pdd/git-gateway/values.yaml"]
