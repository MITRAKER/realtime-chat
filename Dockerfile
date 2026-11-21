# syntax=docker/dockerfile:1

# Build stage: compile one static binary.
FROM golang:1.27-alpine AS build
WORKDIR /src
# Download dependencies first, so this layer is cached until go.mod changes.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/realtime-chat ./cmd/server

# Run stage: a minimal image with no shell or package manager (less to
# attack), running as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/realtime-chat /realtime-chat
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/realtime-chat"]
