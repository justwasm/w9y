#!/usr/bin/env bash

TAG=go1.27.0-go4js.3

[[ -f /go/bin/go ]] || {
  curl -sL https://github.com/justwasm/go/releases/download/${TAG}/${TAG}.linux-amd64.min.tar.gz | tar -xzC /
}

export PATH=/go/bin:/app/bin:$PATH

export CGO_ENABLED=0

export GONOSUMDB='*'

# Prevent excessive OS thread / process creation in container environments (e.g. Railway)
export GOMAXPROCS="${GOMAXPROCS:-2}"

# Increase max user processes limit if allowed by container permissions
ulimit -u unlimited 2>/dev/null || ulimit -u 65535 2>/dev/null || true

type -a go

/bin/go version

go version

tinygo version

w9y server
