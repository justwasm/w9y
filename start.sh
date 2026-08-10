#!/usr/bin/env bash

TAG=go1.27.0-go4js.1

[[ -f /go/bin/go ]] || {
  curl -sL https://github.com/justwasm/go/releases/download/${TAG}/${TAG}.linux-amd64.min.tar.gz | tar -xzC /
}

export PATH=/go/bin:/app/bin:$PATH

export CGO_ENABLED=0

export GONOSUMDB='*'

# TODO: remove this section after go1.27 && compatible tinygo release
command -v go1.27rc2 || {
  go install golang.org/dl/go1.27rc2@latest

  go1.27rc2 download

  ln -sf `which go1.27rc2` `which go`
}

go version

tinygo version

w9y server
