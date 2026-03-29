FROM golang:1.24-alpine AS builder
WORKDIR /workspace

ENV GOTOOLCHAIN=local

COPY ./ ./
RUN go build -ldflags="-s -w" -o goondvr .

FROM alpine:3 AS runnable
RUN apk --no-cache add ca-certificates
WORKDIR /usr/src/app

COPY --from=builder /workspace/goondvr /goondvr

ENTRYPOINT ["/goondvr"]
