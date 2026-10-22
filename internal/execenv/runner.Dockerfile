# kran-runner: the Linux environment of kran's container runtime (SPEC §6.8).
# Toolchains are not baked in: jobs get them from mise, installed on demand
# into the kran-tools volume and shared by every job on the node.
# Built with kran-init (runner-init.sh) next to this file as the context.
FROM debian:stable-slim

ENV DEBIAN_FRONTEND=noninteractive
# Besides the build basics, the everyday command-line tools scripts and
# tests expect on a Linux dev box (envsubst, jq, rg, lsof, sfdisk, nc, dig,
# ss, sqlite3, …). Toolchains (Go, Node, Python, …) come from mise instead.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git build-essential pkg-config \
      bash zsh procps psmisc lsof strace iproute2 iputils-ping netcat-openbsd bind9-dnsutils \
      openssh-client gnupg openssl locales tzdata \
      unzip zip xz-utils zstd bzip2 rsync file less patch diffutils bc time tree xxd bsdextrautils \
      gettext-base jq ripgrep shellcheck sqlite3 fdisk \
 && rm -rf /var/lib/apt/lists/*

# Common UTF-8 locales, so a job sorts and formats like its origin without
# kran-init building one first (others are built per job, about a second).
RUN set -eu; for l in en_US en_GB en_AU en_CA en_IE en_IN en_NZ de_DE de_AT de_CH fr_FR fr_CA es_ES es_MX it_IT nl_NL pt_PT pt_BR pl_PL sv_SE da_DK nb_NO fi_FI cs_CZ ru_RU uk_UA tr_TR ja_JP ko_KR zh_CN zh_TW; do \
      localedef -i "$l" -f UTF-8 "$l.UTF-8"; \
    done

# The workspace is a copy owned by the job's uid, but parent directories
# and $HOME may not be: git must not refuse to work in it.
RUN git config --system --add safe.directory '*'

# mise provisions Go, Node, Python, … per job pin. The Docker CLI with the
# compose and buildx plugins lets tests using `docker`, `docker compose`,
# `docker build` (BuildKit) or testcontainers reach the node's Docker
# through the mounted socket.
# Every download is pinned by version and sha256 (mise and compose: the
# release's published sums; docker: the static archive as published at
# download.docker.com, which has no sums file). Bump version and sums
# together.
ARG MISE_VERSION=v2026.9.18
ARG MISE_SHA256_X86_64=d24fe0bf7e613824ad99f7b8dac3f2b381a37b9f75f84dd250855217095a8de4
ARG MISE_SHA256_AARCH64=2142433ae70decffc5fa24bd160e47931e33f55eb031d128beb97cbae634d168
ARG DOCKER_VERSION=27.5.1
ARG DOCKER_SHA256_X86_64=4f798b3ee1e0140eab5bf30b0edc4e84f4cdb53255a429dc3bbae9524845d640
ARG DOCKER_SHA256_AARCH64=e6b53725a73763ab3f988c73f8772eaed429754c1a579db5ff11f21990fd1817
ARG COMPOSE_VERSION=v2.32.4
ARG COMPOSE_SHA256_X86_64=ed1917fb54db184192ea9d0717bcd59e3662ea79db48bff36d3475516c480a6b
ARG COMPOSE_SHA256_AARCH64=0c4591cf3b1ed039adcd803dbbeddf757375fc08c11245b0154135f838495a2f
ARG BUILDX_VERSION=v0.37.2
ARG BUILDX_SHA256_X86_64=982ca20490b45ed1ec8d99795974d3d874a358f75938c9c237305010e6b7e548
ARG BUILDX_SHA256_AARCH64=efa38cb7aa7db2dbb9ad049b00b0a9737f66f033626177b5a4e845184ad7ab29
RUN set -eu; arch="$(uname -m)"; \
    case "$arch" in \
      x86_64)  mise_arch=x64;   go_arch=amd64; mise_sum=$MISE_SHA256_X86_64;  docker_sum=$DOCKER_SHA256_X86_64;  compose_sum=$COMPOSE_SHA256_X86_64;  buildx_sum=$BUILDX_SHA256_X86_64 ;; \
      aarch64) mise_arch=arm64; go_arch=arm64; mise_sum=$MISE_SHA256_AARCH64; docker_sum=$DOCKER_SHA256_AARCH64; compose_sum=$COMPOSE_SHA256_AARCH64; buildx_sum=$BUILDX_SHA256_AARCH64 ;; \
      *) echo "kran-runner: unsupported architecture $arch" >&2; exit 1 ;; \
    esac; \
    cd /tmp; \
    curl -fsSL -o mise "https://github.com/jdx/mise/releases/download/${MISE_VERSION}/mise-${MISE_VERSION}-linux-${mise_arch}"; \
    echo "$mise_sum  mise" | sha256sum -c -; \
    install -m 0755 mise /usr/local/bin/mise; \
    curl -fsSL -o docker.tgz "https://download.docker.com/linux/static/stable/${arch}/docker-${DOCKER_VERSION}.tgz"; \
    echo "$docker_sum  docker.tgz" | sha256sum -c -; \
    tar -xzf docker.tgz --strip-components=1 -C /usr/local/bin docker/docker; \
    curl -fsSL -o docker-compose "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/docker-compose-linux-${arch}"; \
    echo "$compose_sum  docker-compose" | sha256sum -c -; \
    mkdir -p /usr/local/lib/docker/cli-plugins; \
    install -m 0755 docker-compose /usr/local/lib/docker/cli-plugins/docker-compose; \
    curl -fsSL -o docker-buildx "https://github.com/docker/buildx/releases/download/${BUILDX_VERSION}/buildx-${BUILDX_VERSION}.linux-${go_arch}"; \
    echo "$buildx_sum  docker-buildx" | sha256sum -c -; \
    install -m 0755 docker-buildx /usr/local/lib/docker/cli-plugins/docker-buildx; \
    rm -f mise docker.tgz docker-compose docker-buildx; \
    mise --version; docker --version; docker compose version; docker buildx version

# kran-init gives each job its user, then drops to it; it writes the job's
# toolchain versions to mise.toml and a shim per tool binary to shims/.
COPY kran-init /usr/local/lib/kran/init
RUN chmod 0755 /usr/local/lib/kran/init && mkdir -p /usr/local/lib/kran/shims

ENV MISE_DATA_DIR=/kran/tools/mise \
    MISE_CACHE_DIR=/kran/cache/mise \
    MISE_GLOBAL_CONFIG_FILE=/usr/local/lib/kran/mise.toml \
    MISE_YES=1 \
    PATH=/usr/local/lib/kran/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

LABEL kran.runner="1"

# kran itself (the node's Linux binary, last: it changes most often) serves
# the job's Docker socket, mapping bind mounts of the workspace and home to
# where they live on the Docker host, and trims the shared caches.
COPY kran /usr/local/lib/kran/kran
