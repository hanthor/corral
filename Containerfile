# Corral web UI image — runs `corral web` in-cluster.
# Binaries are cross-compiled/downloaded into build/<arch>/ first (see CI).
# BuildKit supplies TARGETARCH for each requested target platform.
FROM docker.io/library/alpine:3.24
ARG TARGETARCH

# qemu-img powers compressed qcow2 disk exports (the default raw.gz export
# works without it). One RUN step — runs in the target arch, hence the
# --arch/binfmt note above for local cross-builds.
RUN apk add --no-cache qemu-img

COPY --chmod=755 build/${TARGETARCH}/corral   /usr/local/bin/corral
COPY --chmod=755 build/${TARGETARCH}/kubectl  /usr/local/bin/kubectl
COPY --chmod=755 build/${TARGETARCH}/virtctl  /usr/local/bin/virtctl

ENV HOME=/tmp
EXPOSE 8006
ENTRYPOINT ["/usr/local/bin/corral", "web", "--addr", "0.0.0.0:8006"]
