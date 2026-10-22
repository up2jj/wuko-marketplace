FROM golang:1.26-alpine AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -o /sink main.go
FROM alpine:3.23
COPY --from=build /sink /sink
USER 10001:10001
ENTRYPOINT ["/sink"]
