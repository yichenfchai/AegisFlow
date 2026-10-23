FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -S aegis && adduser -S -G aegis aegis
WORKDIR /app
COPY --from=build /out/server /app/server
USER aegis
EXPOSE 8080
ENTRYPOINT ["/app/server"]
