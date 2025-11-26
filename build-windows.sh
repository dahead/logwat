#!/usr/bin/env bash
set -euo pipefail

mkdir -p builds

GOOS=windows GOARCH=amd64 go build -o builds/logwat.exe .
