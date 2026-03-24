FROM golang:1.26-alpine AS build

WORKDIR /app

COPY go.mod go.sum ./

RUN apk --no-cache --no-scripts add ca-certificates git tzdata && \
    go mod download && \
    go generate ./...

COPY . ./

RUN go build -ldflags="-w -s" -tags 'netgo osusergo' -o publish/portainer-deploy-plugin ./cmd/portainer-deploy-plugin 

RUN mkdir -p publish/etc/ssl/certs/ && \
    mkdir -p publish/usr/share/zoneinfo/ && \
    mkdir -p publish/certs/ && \
    mkdir -p publish/static/ && \
    cp /etc/ssl/certs/ca-certificates.crt publish/etc/ssl/certs/ && \
    cp -R /usr/share/zoneinfo publish/usr/share/ && \
    cp -R static/* publish/static/ 2>/dev/null || echo "No static files found"

FROM ghcr.io/wntrtech/scratch:latest
WORKDIR /
COPY --from=build app/publish/ ./
EXPOSE 8080/tcp
ENV TZ=Europe/Riga
ENTRYPOINT ["/portainer-deploy-plugin"]