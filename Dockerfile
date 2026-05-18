FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /prox ./cmd/prox

FROM alpine:3.20
RUN apk add --no-cache ca-certificates go
COPY --from=build /prox /usr/local/bin/prox
ENTRYPOINT ["prox"]
CMD ["serve", "-config", "/etc/prox/config.json5"]
