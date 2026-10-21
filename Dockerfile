# Pinned to 1.25: Go 1.27 retrofitted encoding/json onto encoding/json/v2,
# and PocketBase 0.39's Collection.UnmarshalJSON uses the classic
# "type alias *Collection" trick to avoid recursing into itself - a defined
# pointer type carries no methods under the old encoding/json, but the v2
# implementation resolves the method through the pointer anyway, so every
# collection unmarshal recurses until the goroutine hits Go's 1GB stack
# limit and the process dies with "fatal error: stack overflow".
# Upstream fixed it in PocketBase 0.40.0 (which in turn requires Go 1.27),
# so this pin can go once we upgrade PocketBase.
FROM golang:1.25 AS build
WORKDIR /app
# Modules first, so a source change doesn't invalidate the download layer.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOOS=linux GOARCH=amd64 go build -trimpath -o ingress-plus && mkdir -p /app/pb_data

FROM gcr.io/distroless/base:latest
WORKDIR /app
# Same non-root uid as the website image. PocketBase writes ./pb_data, so
# the directory ships pre-created and owned by that uid for local runs; a
# volume mounted over it must be writable by 65532 too (fsGroup on the
# Deployment).
COPY --from=build --chown=65532:65532 /app/ingress-plus /app/ingress-plus
COPY --from=build --chown=65532:65532 /app/pb_data /app/pb_data
USER 65532:65532
CMD ["/app/ingress-plus"]
