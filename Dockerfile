# syntax=docker/dockerfile:1
FROM ubuntu:26.04@sha256:6a06111c3b0a8600feadb270aec771dc239bf407b25625f93f44ab00e11f4d3c

RUN apt-get update -qq \
    && apt-get install -y --no-install-recommends \
      ca-certificates git curl procps libicu78 libssl3t64 libkrb5-3 zlib1g \
    && rm -r /var/lib/apt/lists \
    && groupadd --gid 10001 ghalet \
    && useradd --uid 10001 --gid ghalet --home-dir /var/lib/ghalet --shell /bin/bash ghalet \
    && install -d -o ghalet -g ghalet -m 0700 \
      /var/lib/ghalet /var/cache/ghalet /var/log/ghalet \
    && install -d -o root -g ghalet -m 0750 /etc/ghalet

ARG TARGETARCH
COPY --from=binaries --chmod=0755 /${TARGETARCH}/ghalet /usr/bin/ghalet
COPY --from=binaries /licenses/ /usr/share/doc/ghalet/

ENV HOME=/var/lib/ghalet
ENV XDG_STATE_HOME=/var/lib XDG_CACHE_HOME=/var/cache
USER ghalet
WORKDIR /var/lib/ghalet
ENTRYPOINT ["/usr/bin/ghalet"]
CMD ["run", "--config", "/etc/ghalet/config.yaml"]
