FROM golang:1.11 AS builder
RUN curl -fsSL -o /usr/local/bin/dep https://github.com/golang/dep/releases/download/v0.4.1/dep-linux-amd64 && chmod +x /usr/local/bin/dep
ENV PKG github.com/mohsenasm/integram-trello
WORKDIR /go/src/${PKG}

COPY Gopkg.toml Gopkg.lock ./

# install locked dependencies(including Integram framework) versions from Gopkg.lock
RUN dep ensure -vendor-only

COPY . ./

RUN CGO_ENABLED=0 GOOS=linux go build -installsuffix cgo -o /go/app ${PKG}/cmd

# move the builded binary into the tiny alpine linux image
FROM alpine:latest
RUN apk --no-cache add ca-certificates && rm -rf /var/cache/apk/*
WORKDIR /app

COPY --from=builder /usr/local/go/lib/time/zoneinfo.zip /usr/local/go/lib/time/zoneinfo.zip
COPY --from=builder /go/app ./

CMD ["./app"]