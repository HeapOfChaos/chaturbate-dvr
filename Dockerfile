FROM golang:1.24-alpine AS builder
WORKDIR /workspace

COPY ./ ./
RUN go build -ldflags=-s -w -o chaturbate-dvr .

FROM alpine:3 AS runnable
RUN apk --no-cache add ca-certificates
WORKDIR /usr/src/app

COPY --from=builder /workspace/chaturbate-dvr /chaturbate-dvr

ENTRYPOINT [/chaturbate-dvr]
