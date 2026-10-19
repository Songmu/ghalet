# syntax=docker/dockerfile:1
FROM ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac

RUN apt-get update -qq \
    && apt-get install -y --no-install-recommends \
      ca-certificates git curl procps libicu74 libssl3t64 libkrb5-3 zlib1g \
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
USER ghalet
WORKDIR /var/lib/ghalet
ENTRYPOINT ["/usr/bin/ghalet"]
CMD ["run", "--config", "/etc/ghalet/config.yaml"]
