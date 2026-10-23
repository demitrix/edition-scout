# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" -o /edition-scout ./cmd/edition-scout  && mkdir /empty

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /edition-scout /edition-scout
COPY LICENSE THIRD_PARTY_NOTICES /licenses/
# /data owned by the nonroot user, so a named volume starts out writable.
COPY --from=build --chown=65532:65532 /empty /data
ENV SCOUT_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/edition-scout"]
