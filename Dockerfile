# Build image
FROM golang:1.25-alpine3.22 AS build

# theATL fork: point GHCR's source/description at our fork, not upstream —
# this image runs modified AGPL code and must not claim upstream WriteFreely
# as its corresponding source. See FORK.md.
LABEL org.opencontainers.image.source="https://github.com/theatl-social/writefreely"
# theATL fork: describe this fork, not upstream, for the same reason.
LABEL org.opencontainers.image.description="theATL.social's fork of WriteFreely, a clean, minimalist publishing platform made for writers, with per-user blog allowances for membership tiers. See FORK.md."

RUN apk -U upgrade \
    && apk add --no-cache nodejs npm make g++ git \
    && npm install -g less less-plugin-clean-css \
    && mkdir -p /go/src/github.com/writefreely/writefreely

WORKDIR /go/src/github.com/writefreely/writefreely

COPY . .

RUN cat ossl_legacy.cnf > /etc/ssl/openssl.cnf

ENV GO111MODULE=on
ENV NODE_OPTIONS=--openssl-legacy-provider

RUN make build \
    && make ui \
    && mkdir /stage \
    && cp -R /go/bin \
      /go/src/github.com/writefreely/writefreely/templates \
      /go/src/github.com/writefreely/writefreely/static \
      /go/src/github.com/writefreely/writefreely/pages \
      /go/src/github.com/writefreely/writefreely/keys \
      /go/src/github.com/writefreely/writefreely/cmd \
      /stage

# Final image
FROM alpine:3.18.4

RUN apk -U upgrade \
    && apk add --no-cache openssl ca-certificates

COPY --from=build --chown=daemon:daemon /stage /go

WORKDIR /go
VOLUME /go/keys
EXPOSE 8080
USER daemon

ENTRYPOINT ["cmd/writefreely/writefreely"]

HEALTHCHECK --start-period=5s --interval=15s --timeout=5s \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/ || exit 1
