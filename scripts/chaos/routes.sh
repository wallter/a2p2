#!/bin/sh
# Make this container's blackhole routes match /chaos/blackhole, one IPv4
# address per line, written by the driver: packets to those addresses are
# dropped here. Both sides of a partition drop the other's addresses.
# Exits non-zero if any route could not be removed or added; the driver
# also reads the routes back before it counts a partition as applied.
fail=0
for addr in $(ip route | awk '$1 == "blackhole" { print $2 }'); do
	ip route del blackhole "$addr" || fail=1
done
if [ -f /chaos/blackhole ]; then
	for addr in $(cat /chaos/blackhole); do
		ip route add blackhole "$addr" || fail=1
	done
fi
exit $fail
