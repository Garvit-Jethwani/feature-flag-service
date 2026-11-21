# Build stage: compile a static binary.
FROM golang:1.27-alpine AS build
WORKDIR /src

# Download dependencies first so this layer is cached until go.mod changes.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# Runtime stage: no shell, no package manager, non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/server"]
