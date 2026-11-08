ARG CADDY_VERSION=2.11.7

FROM docker.io/library/caddy:${CADDY_VERSION}-builder AS builder

WORKDIR /src

COPY adaptive/ ./adaptive/

RUN --network=host xcaddy build \
    --with github.com/local/caddy-adaptive=./adaptive \
    --output /usr/bin/caddy

FROM docker.io/library/caddy:${CADDY_VERSION}

COPY --from=builder /usr/bin/caddy /usr/bin/caddy