# Running the relay

The relay is one static binary with no database. It needs a Linux machine that stays on
and that every agent machine can reach on TCP port 8788.

## On a Linux machine with systemd

```bash
curl -fsSL https://raw.githubusercontent.com/jrimmer/louisiane/main/install.sh | sudo sh
sudo lal relay init
```

`lal relay init` works out the names the machine is reached by (its full name, its short
name, its LAN addresses; override with `--name`, repeated), creates the relay's
certificate, asks for the dashboard password, creates a `louisiane` system user and
installs and starts `louisiane-relay.service`. State lives in `/var/lib/louisiane`:

| File | |
|---|---|
| `relay.json` | The listen address and names, from `relay init`. |
| `dash.hash` | The bcrypt hash of the dashboard password. |
| `hosts.json` | The joined machines and the hashes of their tokens. |
| `tls.crt`, `tls.key` | The self-signed certificate every machine pins. |
| `events.jsonl` | The log: every event, rotated at 64 MB, three kept. |
| `admin.sock` | Host administration (invites, hosts), for root and the relay's user. |

Back up `hosts.json` and `tls.*`: a new certificate means every machine joins again.

Without root or systemd, `lal relay init --state-dir DIR` sets up the same files and tells
you how to start it (`lal relay --state-dir DIR`).

## In Docker

```bash
docker build -t louisiane .
docker run -d --name louisiane --restart unless-stopped -p 8788:8788 \
  -v louisiane:/var/lib/louisiane \
  louisiane relay --state-dir /var/lib/louisiane --name HOSTNAME --name LAN-IP
docker exec -it louisiane lal relay passwd --state-dir /var/lib/louisiane
docker exec louisiane lal invite HOST --state-dir /var/lib/louisiane
```

Name the relay by the host's name and address with `--name`: the container's own are of no
use to agents.

## In a Proxmox LXC

An unprivileged Debian container is plenty (1 core, 256–512 MB, a 2–4 GB disk):

```bash
pct create CTID local:vztmpl/debian-13-standard_13.1-2_amd64.tar.zst \
  --hostname louisiane --rootfs STORAGE:4 --memory 512 --cores 1 \
  --net0 name=eth0,bridge=vmbr0,ip=dhcp --unprivileged 1 --features nesting=1 --onboot 1
pct start CTID
pct enter CTID     # then, inside: the two lines under "On a Linux machine with systemd"
```

Give it a fixed address (a DHCP reservation) and a DNS name before inviting machines; the
names are in the certificate and the invite, though machines pin the certificate itself, so
a later change only means editing `relay` in each machine's `~/.config/louisiane/host.json`.

## Machines and passwords

```bash
sudo lal invite HOST       # on the relay's machine: one line to run on HOST
sudo lal hosts             # the joined machines
sudo lal hosts rm HOST     # cut one off and drop its agents
sudo lal relay passwd      # change the dashboard password; takes effect at once
```

The dashboard is at `https://NAME:8788/` as user `owner`. The browser warns once about the
self-signed certificate; its fingerprint is the pin `lal relay init` printed.

## Upgrading

Install the new binary (`install.sh` again, or copy it to `/usr/local/bin/lal`) and
`systemctl restart louisiane-relay`. Connectors reconnect within seconds, and their agents
are told messages may have been missed meanwhile. Update `lal` on each machine too; running
sessions pick it up when they restart.
