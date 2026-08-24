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
COPY . .
RUN GOOS=linux GOARCH=amd64 go build -trimpath -o ingress-plus

FROM gcr.io/distroless/base:latest
WORKDIR /app
COPY --from=build /app/ingress-plus .
CMD ["/app/ingress-plus"]
