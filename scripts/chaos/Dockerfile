# One a2p2 daemon per container for the chaos harness (scripts/chaos).
# busybox gives a shell, `script` (a pseudo-terminal for the commands that
# ask a person) and `ip` (blackhole routes for partitions) in 1.6 MB.
FROM busybox:1.37.0-musl
COPY a2p2 entry.sh routes.sh /usr/local/bin/
RUN mv /usr/local/bin/entry.sh /usr/local/bin/chaos-entry.sh && \
    mv /usr/local/bin/routes.sh /usr/local/bin/chaos-routes.sh && \
    chmod 755 /usr/local/bin/chaos-entry.sh /usr/local/bin/chaos-routes.sh && \
    mkdir -m 700 /a2p2 /chaos
ENV A2P2_HOME=/a2p2
ENTRYPOINT ["/usr/local/bin/chaos-entry.sh"]
