#!/usr/bin/env bash

## Pushes to PKB private repository, change the registry if you want to push it elsewhere

## Increment this number when you push a new image
VERSION=1
docker build . -t "eu.gcr.io/infra-240614/regproxy2:$VERSION"
docker push "eu.gcr.io/infra-240614/regproxy2:$VERSION"