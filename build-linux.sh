#!/usr/bin/env bash
set -euo pipefail

mkdir -p builds

GOOS=linux GOARCH=amd64 go build -o builds/logwat .
