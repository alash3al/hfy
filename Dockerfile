FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/hfy .

# Alpine, not distroless: local MCP servers are commands that must exist in the
# image. Extend this stage (apk add / COPY) with the ones your config uses.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 hfy
COPY --from=build /out/hfy /usr/local/bin/hfy
USER hfy
EXPOSE 8080
ENV HFY_CONFIG=/etc/hfy/hfy.yaml
ENTRYPOINT ["hfy"]
CMD ["serve", "--work"]
