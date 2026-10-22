# kran-runner: the Linux environment of kran's container runtime (SPEC §6.8).
# Toolchains are not baked in: jobs get them from mise, installed on demand
# into the kran-tools volume and shared by every job on the node.
FROM debian:stable-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git build-essential unzip xz-utils pkg-config \
      bash zsh procps openssh-client gnupg \
 && rm -rf /var/lib/apt/lists/*

# mise provisions Go, Node, Python, … per job pin.
RUN curl -fsSL https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh \
 && mise --version

# Docker CLI + compose plugin, so tests using `docker`, `docker compose` or
# testcontainers reach the node's Docker through the mounted socket.
ARG DOCKER_VERSION=27.5.1
ARG COMPOSE_VERSION=v2.32.4
RUN arch="$(uname -m)" \
 && curl -fsSL "https://download.docker.com/linux/static/stable/${arch}/docker-${DOCKER_VERSION}.tgz" \
    | tar -xz --strip-components=1 -C /usr/local/bin docker/docker \
 && mkdir -p /usr/local/lib/docker/cli-plugins \
 && curl -fsSL -o /usr/local/lib/docker/cli-plugins/docker-compose \
    "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-${arch}" \
 && chmod +x /usr/local/lib/docker/cli-plugins/docker-compose

ENV MISE_DATA_DIR=/kran/tools/mise \
    MISE_CACHE_DIR=/kran/cache/mise \
    MISE_YES=1 \
    PATH=/kran/tools/mise/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

LABEL kran.runner="1"
