#!/bin/sh
set -e
apk add --update go mercurial
mkdir -p /go/src/github.com/gliderlabs
cp -r /src /go/src/github.com/gliderlabs/logspout
cd /go/src/github.com/gliderlabs/logspout
export GOPATH=/go
#go get
GO15VENDOREXPERIMENT=1 go build -ldflags "-X main.Version $1" -o /bin/logspout
apk del go mercurial
rm -rf /go
rm -rf /var/cache/apk/*

# backwards compatibility
ln -fs /tmp/docker.sock /var/run/docker.sock
