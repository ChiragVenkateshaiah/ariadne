# Multi-stage build: compile in a full Go image, ship a static binary on
# distroless. Small images matter here for reasons beyond disk space -- kind
# nodes pull into a shared containerd store, and a fast image build/load loop
# is what lets the demo redeploy a "changed" service in seconds, not minutes.
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY sut/go.mod sut/go.sum ./
RUN go mod download
COPY sut/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/search-api ./services/search-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/search-api /search-api
USER nonroot:nonroot
ENTRYPOINT ["/search-api"]
