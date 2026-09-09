FROM golang:1.26.7-alpine AS build

WORKDIR /src

# Dependencies are cached in a layer of their own, separate from the sources.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/quotes .

FROM alpine:3.23

# ca-certificates are needed to reach the rate provider API.
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 quotes

COPY --from=build /out/quotes /usr/local/bin/quotes

USER quotes
EXPOSE 8080

ENTRYPOINT ["quotes"]
