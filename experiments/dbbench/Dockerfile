# The scorecard harness in a Linux container, so RepoDB runs on the same Docker
# VM, kernel and storage stack as the MySQL and Dolt baselines. Build from the
# repository root (`make bench-docker` does): docker build -f
# experiments/dbbench/Dockerfile .

FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -tags gms_pure_go -o /out/dbbench ./experiments/dbbench

FROM debian:trixie-slim
# The harness and RepoDB drive the git executable; its version is part of the
# measurement and is recorded in every report.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 bench \
    && mkdir -p /fixtures /out \
    && chown bench:bench /fixtures /out
COPY --from=build /out/dbbench /usr/local/bin/dbbench
USER bench
# RepoDB's fixture repositories need a Git identity for their commits.
RUN git config --global user.name "RepoDB scorecard" \
    && git config --global user.email "scorecard@repodb.invalid"
WORKDIR /fixtures
ENTRYPOINT ["dbbench"]
