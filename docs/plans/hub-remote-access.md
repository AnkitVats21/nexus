# Reaching home hubs from anywhere (Lightsail directory + relay)

Status: requirements agreed 2026-11-02 (interview), design not started.
Replaces the Lightsail pilot hub (`lightsail-pilot.md`): the server stops
being a hub and becomes a directory and relay for the hubs at home.

## Goal

Several hubs run at home (Pi, HP laptop, more later). Lightsail knows which
are online. After one login you see the list, pick a hub, give its password,
and Lightsail opens it: directly over IPv6 when the client can, through a
relay on Lightsail when it can't.

## Requirements

1. **Directory on Lightsail.** Each hub keeps an outgoing connection to
   Lightsail and reports that it is online and its current IPv6 name.
2. **Login and list.** One Lightsail login (the owner's) shows the hubs that
   are online. Strangers see nothing, not even hub names.
3. **Opening a hub.** Hub name + that hub's own password. The hub itself
   checks the password (not only Lightsail), since it will be reachable
   directly.
4. **Clients.**
   - Browser dashboard (from a phone on mobile data, another network, ...).
   - The board when away from home.
   - Orbit satellites on another network, as music outputs.
   - Later: voice from a phone, as a web page using the browser's mic (no
     app).
5. **Path: IPv6 direct, relay fallback.**
   - Clients with IPv6 connect straight to `<hub>.hub.wiztex.in`: DNS-only
     record (no Cloudflare proxy), AAAA kept up to date by the hub,
     Let's Encrypt certificate via the Cloudflare DNS API.
   - Clients without IPv6 go through Lightsail, which passes the traffic to
     the hub over the hub's outgoing connection.
   - Lightsail is reached directly, not through the Cloudflare proxy: the
     proxy added ~300 ms per round trip (direct ~25 ms).
6. **Board away from home.** Local network first (mDNS, as today); if no hub
   is found, the hub picked for it in the dashboard (name + password saved on
   the board). Direct needs IPv6 autoconfig turned on in the firmware (not
   on today; measure RAM before keeping it).
7. **Single owner.** Only the owner uses the hubs: no accounts, no sharing
   (see `hub-multi-user.md` for that later).
8. **Retire the pilot hub** on Lightsail; the server only lists and relays.

## Proposed (confirm in the design)

- The board needs only the hub name + password, not the Lightsail login (the
  login is for the list).
- One password per hub; the board keeps its existing hub token alongside.
- Cloudflare API token limited to the hub.wiztex.in DNS records; nginx and
  certbot steps on Lightsail are run by the owner.

## Step 1: the directory, LAN links only (built 2026-11-03)

The least-change first step: a list of the hubs, the ones on your current
network first, each opening the hub at its LAN address. No relay, no
IPv6 names, no new DNS or certificates.

- **Directory** (`pkg/hub/directory.go`, `nexus-hub --directory`): a second,
  small instance of the hub binary on Lightsail (127.0.0.1:8381,
  `packaging/systemd/nexus-directory.service`), next to the pilot hub,
  which stays as it is. nginx (`packaging/nginx/hub-directory.conf`) routes
  only `/hub/directory` (no login) and
  `/hub/directory/announce` to it.
- **Announce** (`pkg/hub/announce.go`, `--announce-url`,
  `--announce-token-file`, `--name`): each home hub POSTs
  `{name, lan_url, version}` every minute, once over IPv4 and once over
  IPv6, with a token from the directory's token file. The LAN URL is
  `http://<first private IPv4>:<port>/` (or `--announce-lan-url`), so a new
  DHCP address shows up within a minute.
- **"On this network"**: the viewer's public address (CF-Connecting-IP via
  X-Real-IP, trusted from loopback only) equals the hub's public IPv4, or
  shares its IPv6 /64. No login (owner's choice, 2026-11-03): a viewer sees
  only the hubs on their own network, never the others. Someone behind the
  same CGNAT address would see a hub's name and private address, of no use
  outside the home. `--directory-all` lists every hub (put a login in front).
- In memory only: online while heard from within 3 min; forgotten after 7
  days; a directory restart empties the list for a minute.
- Limits: plain `http://` to the hub (no secure context: no mic in the
  browser); hubs on other networks aren't shown (reaching them is the relay,
  the later step).

## Security baseline (done 2026-11-02)

Both home routers pass incoming IPv6, and the hubs answered from the
internet with no password: dashboard, the `/api` proxy to the board,
`/hub/bluetooth`. Port 8380 is now firewalled on both hubs (nftables table
`inet nexus_hub`: only loopback, Tailscale, private IPv4, link-local and ULA
IPv6; everything else gets a TCP reset). Open it to the internet again only
once requirement 3 is in place.

- HP: `/etc/nexus-hub/firewall.nft`, loaded by
  `nexus-hub.service.d/firewall.conf` (`ExecStartPre`).
- Pi: `/etc/nexus-hub-firewall.nft`, loaded by
  `/etc/local.d/nexus-hub-firewall.start`; saved with `lbu commit`.
