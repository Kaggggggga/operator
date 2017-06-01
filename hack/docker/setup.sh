#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

GOPATH=$(go env GOPATH)
SRC=$GOPATH/src
BIN=$GOPATH/bin
ROOT=$GOPATH
REPO_ROOT=$GOPATH/src/github.com/k8sdb/operator

source "$REPO_ROOT/hack/libbuild/common/kubedb_image.sh"

APPSCODE_ENV=${APPSCODE_ENV:-dev}
IMG=operator

DIST=$GOPATH/src/github.com/k8sdb/operator/dist
mkdir -p $DIST
if [ -f "$DIST/.tag" ]; then
	export $(cat $DIST/.tag | xargs)
fi

clean() {
    pushd $REPO_ROOT/hack/docker
    rm -f operator Dockerfile
    popd
}

build_binary() {
    pushd $REPO_ROOT
    ./hack/builddeps.sh
    ./hack/make.py build
    detect_tag $DIST/.tag
    popd
}

build_docker() {
    pushd $REPO_ROOT/hack/docker
    cp $DIST/operator/operator-linux-amd64 operator
    chmod 755 operator

    cat >Dockerfile <<EOL
FROM alpine

RUN set -x \
    && apk update \
    && apk add ca-certificates \
    && rm -rf /var/cache/apk/*

COPY operator /operator

USER nobody:nobody
ENTRYPOINT ["/operator"]
EOL
    local cmd="docker build -t kubedb/$IMG:$TAG ."
    echo $cmd; $cmd

    rm operator Dockerfile
    popd
}

build() {
    build_binary
    build_docker
}

docker_push() {
    if [ "$APPSCODE_ENV" = "prod" ]; then
        echo "Nothing to do in prod env. Are you trying to 'release' binaries to prod?"
        exit 1
    fi
    if [ "$TAG_STRATEGY" = "git_tag" ]; then
        echo "Are you trying to 'release' binaries to prod?"
        exit 1
    fi
    hub_canary
}

docker_release() {
    if [ "$APPSCODE_ENV" != "prod" ]; then
        echo "'release' only works in PROD env."
        exit 1
    fi
    if [ "$TAG_STRATEGY" != "git_tag" ]; then
        echo "'apply_tag' to release binaries and/or docker images."
        exit 1
    fi
    hub_up
}

source_repo $@