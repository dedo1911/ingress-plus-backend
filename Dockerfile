FROM golang:1 AS build
WORKDIR /app
COPY . .
RUN GOOS=linux GOARCH=amd64 go build -trimpath -o ingress-plus

FROM gcr.io/distroless/base:latest
WORKDIR /app
COPY --from=build /app/ingress-plus .
CMD ["/app/ingress-plus"]
