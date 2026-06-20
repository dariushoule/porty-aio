# Pinned Go toolchain for reproducible, dependency-free cross-compilation.
#
# This image only provides the toolchain; source is mounted at run time by the
# build scripts, so editing code does not require rebuilding this image.
# To upgrade Go for every target, bump this single line.
FROM golang:1.26-alpine

# git: optional version stamping. ca-certificates: in case deps appear later.
# upx: post-build binary compression to minimize artifact size.
RUN apk add --no-cache git ca-certificates upx

WORKDIR /src
