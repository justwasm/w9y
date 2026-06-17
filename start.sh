#!/usr/bin/env bash

curl -sL https://github.com/justwasm/go/releases/download/go1.27.0-wanix.6/go1.27.0-wanix.6.linux-amd64.min.tar.gz | tar -xzC /

export PATH=/go/bin:/app/bin:$PATH

export CGO_ENABLED=0

export GONOSUMDB='*'

go version

tinygo version

w9y server
