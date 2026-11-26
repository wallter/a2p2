# Image of the network benchmark (scripts/bench-net), for its containers
# only; the product ships no image. Alpine for tc (iproute2) and netem,
# which shape the link inside each daemon's container. The binaries are
# built by bench_net.py: a2p2 (the CLI, ./cmd/a2p2), a2p2-netd (the same
# daemon with its log at Debug, ./internal/bench/netd) and a2p2-netload
# (the load driver, ./internal/bench/netload).
ARG BASE=alpine:3.24
FROM ${BASE}
RUN apk add --no-cache iproute2-tc
COPY a2p2 a2p2-netd a2p2-netload /usr/local/bin/
RUN chmod 755 /usr/local/bin/a2p2 /usr/local/bin/a2p2-netd /usr/local/bin/a2p2-netload && mkdir -m 700 /a2p2
ENV A2P2_HOME=/a2p2
