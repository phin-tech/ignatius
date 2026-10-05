FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/phin-tech/ignatius/go/cli.Version=${VERSION}" -o /out/ignatius ./cmd/ignatius

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ignatius /ignatius
ENTRYPOINT ["/ignatius"]
CMD ["serve", "--config", "/etc/ignatius/ignatius.toml"]
