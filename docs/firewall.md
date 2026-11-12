# Keeping jobs off your network

This page gives the two host firewall rules that keep manade's jobs away from the host and your local network. Add them before the pool runs its first job.

## Why the rules are yours to add

A job runs code from its repository and from every dependency it installs, so treat it as untrusted. manade puts each job on its own Docker network, `manade-isolated`. Its bridge interface is `manade0`, and its subnet is `network.subnet`, `172.30.0.0/24` by default. Jobs on that network cannot reach each other. Docker filters no outgoing traffic, though, so without these rules a job can open any service on the host or your LAN.

Two rules are needed, because the kernel handles the two kinds of traffic in different places:

- Traffic to another machine passes through the host's forward path. The first rule drops what a job sends to private, link-local and shared address ranges, and lets replies through.
- Traffic to one of the host's own addresses goes to the host's input path. The second rule drops everything a job starts there.

A job still reaches the internet, as it must to fetch actions, packages and tools.

The names change with your settings. With `POOL_NAME` set, the network is `<POOL_NAME>-isolated` and the interface `<POOL_NAME>0`. A profile with `network: bridge` uses a second network, `<POOL_NAME>-bridge`, on interface `<POOL_NAME>1` and `network.bridge_subnet`. Its jobs reach each other, and the same two rules keep them off the host and the LAN. The rules below cover both interfaces. An interface no profile uses yet costs nothing.

manade cannot see the host's firewall from its container, so it cannot tell you whether the rules are in place. At start it logs Docker's firewall backend, `iptables` or `nftables`, in a line that begins `Docker answered`. Use the matching set below. An older Docker does not report its backend, and the line says `unknown`. Then add both sets, as the line says. Such a Docker predates the nftables backend, which arrived in Docker 29, so its own rules go through iptables.

## The iptables backend

Run these as root on the host. Add your own public addresses to `MANADE-JOBS` with more `DROP` lines if you host services on them.

```sh
# Traffic a job sends to another machine: replies pass, private ranges are dropped.
iptables -N MANADE-JOBS
iptables -A MANADE-JOBS -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN
iptables -A MANADE-JOBS -d 10.0.0.0/8 -j DROP
iptables -A MANADE-JOBS -d 100.64.0.0/10 -j DROP
iptables -A MANADE-JOBS -d 169.254.0.0/16 -j DROP
iptables -A MANADE-JOBS -d 172.16.0.0/12 -j DROP
iptables -A MANADE-JOBS -d 192.168.0.0/16 -j DROP
iptables -I DOCKER-USER -i manade0 ! -o manade0 -j MANADE-JOBS
iptables -I DOCKER-USER -i manade1 ! -o manade1 -j MANADE-JOBS

# Traffic a job sends to the host itself: only replies pass.
iptables -N MANADE-HOST
iptables -A MANADE-HOST -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
iptables -A MANADE-HOST -j DROP
iptables -I INPUT -i manade0 -j MANADE-HOST
iptables -I INPUT -i manade1 -j MANADE-HOST
```

These rules last until the host restarts. Save them with your distribution's tool, such as `netfilter-persistent save` on Debian and Ubuntu. They must load after Docker starts, because Docker creates the `DOCKER-USER` chain.

## The nftables backend

Docker's nftables backend has no `DOCKER-USER` chain. Your own table runs beside Docker's, and a drop in any table is final. Save this as a file, such as `/etc/nftables.d/manade.nft`, and load it with `nft -f /etc/nftables.d/manade.nft`:

```text
table inet manade {
	set private4 {
		type ipv4_addr
		flags interval
		elements = { 10.0.0.0/8, 100.64.0.0/10, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16 }
	}

	# Traffic a job sends to another machine: replies pass, private ranges are dropped.
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname { "manade0", "manade1" } ct state established,related accept
		iifname "manade0" oifname != "manade0" ip daddr @private4 drop
		iifname "manade1" oifname != "manade1" ip daddr @private4 drop
	}

	# Traffic a job sends to the host itself: only replies pass.
	chain input {
		type filter hook input priority filter; policy accept;
		iifname { "manade0", "manade1" } ct state established,related accept
		iifname { "manade0", "manade1" } drop
	}
}
```

Include the file from `/etc/nftables.conf` so it loads at every start.

## DNS

A job needs a resolver. Unset, `network.dns` leaves jobs on the resolvers Docker hands its containers. Docker asks a loopback resolver on the host from the host's side, and that works with both rules in place. Docker asks any other resolver from the job's own address, so the rules drop it when it sits on your LAN, as a home router's resolver does.

So when the host's resolver is a LAN address, set `network.dns` to a resolver the jobs may use:

- A public resolver needs no rule change, because the rules drop only private ranges.
- A resolver on your LAN needs one more rule before the drops. Put its address in place of `RESOLVER` below. With the iptables set, add both lines. With the nftables set, add the `accept` line to the `forward` chain, after the `established,related` line.

```sh
iptables -I MANADE-JOBS 2 -d RESOLVER -p udp --dport 53 -j RETURN
iptables -I MANADE-JOBS 2 -d RESOLVER -p tcp --dport 53 -j RETURN
```

```text
		iifname { "manade0", "manade1" } ip daddr RESOLVER meta l4proto { tcp, udp } th dport 53 accept
```

A resolver that runs on the host itself, at one of its own addresses, needs the same exception in the input rules.

## Checking the rules

Run this workflow from a repository the pool serves, on the `default` profile. Set `HOST_TARGET` to a port open on the host, at the job network's first address, and `LAN_TARGET` to a LAN address and port that answer. The run fails until `LAN_TARGET` is set. Use open ports, because a closed one fails the connection with or without the rules.

```yaml
name: Firewall check
on: workflow_dispatch
jobs:
  check:
    runs-on: default
    env:
      HOST_TARGET: 172.30.0.1:22
      LAN_TARGET: ""
    steps:
      - name: The host and the LAN refuse the job
        run: |
          [ -n "$LAN_TARGET" ] || { echo "Set LAN_TARGET to a LAN address and port that answer"; exit 1; }
          for target in "$HOST_TARGET" "$LAN_TARGET"; do
            if timeout 5 bash -c "exec 3<>/dev/tcp/${target%:*}/${target#*:}"; then
              echo "The job reached $target"
              exit 1
            fi
          done
      - name: Names still resolve
        run: getent hosts github.com
```

The run passes when both steps pass. A failure in the first step names the address a job could reach.
