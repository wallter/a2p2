#!/bin/sh
# Container entry point of the chaos harness: apply the partition routes
# the driver wrote for this container, then become the daemon (exec keeps
# it the main process, so `docker kill -s KILL` is a kill -9 of it). If the
# routes cannot be applied the container stops, and the driver counts the
# start as failed.
/usr/local/bin/chaos-routes.sh || exit 1
exec /usr/local/bin/a2p2 daemon
