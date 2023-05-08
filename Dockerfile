FROM golang:1.18 AS builder

ENV PKG github.com/mohsenasm/integram-trello
RUN mkdir /app
WORKDIR /app

COPY go.mod go.sum ./

# install the dependencies without checking for go code
RUN go mod download

COPY . ./

RUN CGO_ENABLED=0 GOOS=linux go build -installsuffix cgo -o /go/app ${PKG}/cmd

# move the builded binary into the tiny alpine linux image
FROM alpine:latest
RUN apk --no-cache add ca-certificates && rm -rf /var/cache/apk/*
WORKDIR /app

COPY --from=builder /usr/local/go/lib/time/zoneinfo.zip /usr/local/go/lib/time/zoneinfo.zip
COPY --from=builder /go/app ./

CMD ["./app"]