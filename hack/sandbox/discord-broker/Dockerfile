FROM golang:1.23-alpine AS build
WORKDIR /src
COPY main.go .
RUN go build -o /discord-broker main.go
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /discord-broker /discord-broker
EXPOSE 8080
ENTRYPOINT ["/discord-broker"]
