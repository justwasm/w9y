#!/usr/bin/env bash

TAG=go1.27.0-go4js.1

curl -sL https://github.com/justwasm/go/releases/download/${TAG}/${TAG}.linux-amd64.min.tar.gz | tar -xzC /

export PATH=/go/bin:/app/bin:$PATH

export CGO_ENABLED=0

export GONOSUMDB='*'

go version

tinygo version

w9y server
