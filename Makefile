TARGETOS ?= linux
TARGETARCH ?= amd64

ifneq ($(CI_COMMIT_TAG),)
	VERSION ?= $(subst v,,$(CI_COMMIT_TAG))
else
	VERSION ?= development
endif
LDFLAGS := -s -w -extldflags "-static" -X "main.Version=$(VERSION)"

build:
	CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags '${LDFLAGS}' -v -a -tags netgo -o portainer-deploy-plugin ./cmd/portainer-deploy-plugin
