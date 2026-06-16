#!/usr/bin/env bash

curl -sL https://github.com/justwasm/go/releases/download/go1.27.0-wanix.6/go1.27.0-wanix.6.linux-amd64.min.tar.gz | tar -xvzC /

export PATH=/go/bin:$PATH

w9y server
