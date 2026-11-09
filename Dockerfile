# Image built by GoReleaser from the release binary (see .goreleaser.yaml).
#   docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/work" ghcr.io/everlasting18/sb2pb migrate
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/sb2pb /usr/local/bin/sb2pb
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/sb2pb"]
