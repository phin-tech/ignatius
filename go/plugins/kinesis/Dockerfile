# The gateway with the Kinesis event sink compiled in. Build with the go/ directory as the
# context, because this module builds against the core checkout next to it:
#
#   docker build -f plugins/kinesis/Dockerfile -t ignatius-kinesis .      (from go/)
FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY . .
WORKDIR /src/plugins/kinesis
RUN go mod download
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/phin-tech/ignatius/go/cli.Version=${VERSION}" -o /out/ignatius ./cmd/ignatius

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ignatius /ignatius
ENTRYPOINT ["/ignatius"]
CMD ["serve", "--config", "/etc/ignatius/ignatius.toml"]
