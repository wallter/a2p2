#!/bin/sh
# Make this container's blackhole routes match /chaos/blackhole, one IPv4
# address per line, written by the driver: packets to those addresses are
# dropped here. Both sides of a partition drop the other's addresses.
ip route | grep '^blackhole' | while read -r _ addr _; do
	ip route del blackhole "$addr"
done
[ -f /chaos/blackhole ] || exit 0
while read -r addr; do
	[ -n "$addr" ] && ip route add blackhole "$addr"
done </chaos/blackhole
exit 0
