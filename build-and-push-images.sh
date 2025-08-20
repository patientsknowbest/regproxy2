#!/usr/bin/env bash

## Pushes to PKB private repository, change the registry if you want to push it elsewhere

## Increment this number when you push a new image
VERSION=7
docker build . -t "europe-docker.pkg.dev/infra-240614/eu.gcr.io/regproxy2:$VERSION"
docker push "europe-docker.pkg.dev/infra-240614/eu.gcr.io/regproxy2:$VERSION"
