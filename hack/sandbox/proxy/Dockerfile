FROM golang:1.23-alpine AS build
WORKDIR /src
COPY main.go .
RUN go build -o /proxy main.go
FROM alpine:3.20
COPY --from=build /proxy /proxy
EXPOSE 8888
ENTRYPOINT ["/proxy"]
