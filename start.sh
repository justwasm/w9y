#!/usr/bin/env bash

TAG=go1.27.0-go4js.2

[[ -f /go/bin/go ]] || {
  curl -sL https://github.com/justwasm/go/releases/download/${TAG}/${TAG}.linux-amd64.min.tar.gz | tar -xzC /
}

export PATH=/go/bin:/app/bin:$PATH

export CGO_ENABLED=0

export GONOSUMDB='*'

type -a go

/bin/go version

go version

tinygo version

w9y server
