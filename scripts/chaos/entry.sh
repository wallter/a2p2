#!/bin/sh
# Container entry point of the chaos harness: apply the partition routes
# the driver wrote for this container, then become the daemon (exec keeps
# it the main process, so `docker kill -s KILL` is a kill -9 of it).
/usr/local/bin/chaos-routes.sh
exec /usr/local/bin/a2p2 daemon
