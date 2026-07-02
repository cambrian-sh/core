# PageRank recompute worker (ADR-0054 D2) — the always-up container that keeps
# chunk_pagerank fresh, decoupled from the intermittent kernel.
# Build context is the repo root: docker build -f cmd/pagerank-recompute/Dockerfile .
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/pagerank-recompute ./cmd/pagerank-recompute

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pagerank-recompute /pagerank-recompute
COPY --from=build /src/configs /configs
ENV CAMBRIAN_CONFIG=/configs/config.json
ENTRYPOINT ["/pagerank-recompute"]
