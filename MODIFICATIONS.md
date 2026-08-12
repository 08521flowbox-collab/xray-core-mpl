# Modifications relative to upstream Xray-core

Base: [`XTLS/Xray-core`](https://github.com/XTLS/Xray-core) tag **`v1.260327.0`** (commit `d2758a0`).

This fork exists for exactly one reason: upstream Xray-core is MPL-2.0, but it depends on
`github.com/sagernet/sing` and `github.com/sagernet/sing-shadowsocks`, both of which are
**GPL-3.0-or-later**. Linking them into an application makes the whole binary subject to
the GPL. This fork removes those dependencies so the resulting library is MPL-2.0 only.

The fork is still MPL-2.0. Every file changed below is Covered Software and its source is
published here.

## Removing `github.com/sagernet/sing`

| File | Change |
|---|---|
| `common/control/interface.go` | **New.** Local definition of `control.Func`, previously taken from `sing/common/control`. It is a type alias of the standard library's `net.ListenConfig.Control` signature, so it is interchangeable with the original. |
| `transport/internet/system_dialer.go` | Import `sing/common/control` → `xray-core/common/control`. |
| `transport/internet/system_listener.go` | Same. |
| `transport/internet/system_listener_test.go` | Same. |
| `app/proxyman/outbound/uot.go` | **Deleted.** Implemented UDP-over-TCP on top of `sing/common/uot`, which is real GPL protocol code, not an alias. |
| `app/proxyman/outbound/handler.go` | Removed the `getUoTConnection` call and the now-unused `os` import. UoT only ever triggered for a destination domain equal to `uot.MagicAddress` / `uot.LegacyMagicAddress`, i.e. only for Shadowsocks-2022 clients, which this fork does not ship. |

## Removing `github.com/sagernet/sing-shadowsocks`

Shadowsocks-2022 cannot be implemented without it, so the protocol is dropped. Legacy
Shadowsocks (AEAD) is untouched and still works.

| File | Change |
|---|---|
| `proxy/shadowsocks_2022/` | **Deleted** (whole package). |
| `common/singbridge/` | **Deleted** (whole package). It existed only to adapt sing's interfaces for `proxy/shadowsocks_2022`; nothing else imported it. |
| `infra/conf/shadowsocks.go` | Dropped both sing imports and `buildShadowsocks2022`. `shadowaead_2022.List` is replaced by a local `shadowsocks2022Methods` list used solely to reject a 2022 method name with a clear error instead of silently mis-parsing it as a legacy cipher. |
| `main/commands/all/api/inbound_user_add.go` | Removed the `shadowsocks_2022.MultiUserServerConfig` case. |
| `testing/scenarios/shadowsocks_2022_test.go` | **Deleted.** |

## Removing `proxy/wireguard`

`proxy/wireguard/tun_linux.go` imports `sing/common/control`. WireGuard is not used by the
consumer of this fork, and keeping it would mean maintaining another sing-free port, so the
proxy is dropped rather than patched.

| File | Change |
|---|---|
| `proxy/wireguard/` | **Deleted** (whole package). |
| `infra/conf/wireguard.go`, `infra/conf/wireguard_test.go` | **Deleted.** |
| `infra/conf/xray.go` | Removed the two `"wireguard"` inbound/outbound config registrations. |
| `main/distro/all/all.go` | Removed the `proxy/wireguard` blank import. |
| `testing/scenarios/wireguard_test.go` | **Deleted.** |

Note that `transport/internet/finalmask/header/wireguard` is a different, unrelated package
(a packet header obfuscator) and is untouched.

## Fixing the tun inbound's teardown

Not a licence change, and the only change here that alters behaviour. Upstream never closes
`proxy/tun`, so the interface it attached to stays referenced after the instance that owns
it is gone.

The path that should reach it does not exist: `proxy/tun.Handler.Network()` returns an empty
list because the tun is not a listener, so `NewAlwaysOnInboundHandler` builds no worker for
it — and a worker is the only caller of `common.Close` on a proxy. `Handler` also had no
`Close` method, so even when reached it would have been a no-op through `common.Close`'s
`Closable` type check. Both halves had to change.

| File | Change |
|---|---|
| `proxy/tun/handler.go` | **New `Close() error`**, implementing `common.Closable`: closes the stack, then the tun. The stack goes first because its `Close` reaches gVisor's `endpoint.Attach(nil)`, which stops the packet dispatchers and waits for their goroutines to leave `dispatchLoop`; releasing the tun before them would leave them reading a descriptor already let go. Idempotent under a mutex — teardown has several entrances, and a failed `Init` closes what it built. `Init` now keeps the `Tun` it creates, which it previously dropped on the floor. |
| `app/proxyman/inbound/always.go` | `Close` now calls `common.Close(h.proxy)` **when the handler has no workers**. Guarded on the count so an inbound that does have workers keeps closing its proxy exactly once, by the pre-existing path. |

Why it matters on the consumer's platform: gVisor's `fdbased` dispatchers sit in `poll()` on
the descriptor, so the kernel holds a reference to the tun file and the descriptor's owner
closing it does not bring the interface down. On Android, `system_server` tears down the VPN
network — and the status bar key icon with it — from `Vpn.interfaceRemoved`, which never
fires while the interface is still referenced. Measured before this change: the interface
outlived the disconnect by 4.7 s, by over 25 s, and once by 31 s, the wait being however long
it took for some packet to wake the poll. Every disconnect also leaked the whole stack — NIC,
endpoint and forwarder goroutines.

Neither `fdbased.endpoint.Close()` nor `AndroidTun.Close()` is a fallback: both are empty
implementations upstream.

## Making the routing table replaceable while the core is running

Not a licence change. Like the tun teardown above, this is a behaviour fix the consumer needs.

`ReloadRules` already existed and already replaced the whole rule set — but it was written for
a caller that basically never runs. Upstream reaches it only through the gRPC routing API
(`app/router/command`), which most builds do not compile in, and which nobody drives from a UI.
The consumer of this fork drives it from one: the user edits their routing rules and the new
set has to take effect on the next connection, without restarting the core and without dropping
the connections that are already up. That turns three latent defects into real ones.

| File | Change |
|---|---|
| `app/router/router.go` | `rules`, `balancers` and `domainStrategy` moved out of `Router` into a new **`snapshot`** struct held in an `atomic.Pointer[snapshot]`. Snapshots are built whole and published with one store; they are never mutated after publication. `pickRouteInternal` loads once at the top and reads only from that load. `Init`, `ReloadRules` and `RemoveRule` build a replacement and store it. `mu` now serialises writers against each other only. `closeWebhooks` became a free function over a rule slice, and `RuleExists` a thin wrapper over a new `ruleExists(rules, tag)`. |
| `app/router/balancing.go`, `app/router/balancing_override.go` | `r.balancers` → `r.load().balancers` (four read-only lookups). |

**1. `PickRoute` raced `ReloadRules`.** `pickRouteInternal` walked `r.rules` holding nothing,
while `ReloadRules` reassigned and appended to that same field under `mu`. A plain data race,
reproduced by `TestReloadRacesPickRoute` in `app/router/router_reload_test.go`, which fails
under `-race` on the unfixed router.

**Why not a read lock.** This is the part worth spelling out, because `RWMutex` is the obvious
fix and it is the wrong one here. Under `IpIfNonMatch`, `pickRouteInternal` walks the rules a
second time *after* `routing_dns.ContextWithDNSClient`, and that second walk resolves the
destination for real — on this consumer's platform the query goes out through the tunnel, so
tens to hundreds of milliseconds, occasionally a timeout. A reader holding `RLock` across it
holds it for that whole time. Worse, Go's `RWMutex` blocks *new* readers as soon as a writer is
waiting, so a single reload arriving mid-lookup stalls every new connection behind that one DNS
query. The user pressing Save on a rule edit would freeze their own traffic. Copy-on-write costs
readers one atomic load (a single `LDAR` on arm64), is wait-free, and never blocks on a writer.

**2. A failed reload left a mutilated rule set.** With `shouldAppend == false`, upstream closed
the live webhooks and emptied `rules` and `balancers` *before* building anything, then bailed out
mid-loop on the first bad rule — leaving the router live with however many rules it had managed
to compile, and with the old webhooks already closed. There is no way to put that back. Since
nothing is published until the whole set is built, a rejected config now changes nothing at all;
old webhooks are closed only after the store succeeds. `TestFailedReloadKeepsThePreviousRules`.

One related correction: the duplicate-`ruleTag` check called `r.RuleExists`, which read the
*live* set. With the live set emptied first, two rules sharing a tag inside one config were not
duplicates of anything and both got loaded. The check now runs against the set being built.

**3. `domainStrategy` was unreachable after `Init`.** `ReloadRules` never touched it, so a
running core kept whatever strategy it started with. This fork needs it: address rules
(`geoip:` / CIDR) cannot match a sniffed domain target unless the strategy is `IpIfNonMatch`,
and paying for a DNS resolution per unmatched connection is not something to leave on when the
user's rules are all domains. A full replacement now takes `config.DomainStrategy`; an append
keeps the live one, because an appended config carries the zero value (`AsIs`) whether or not
its author meant to say anything about the strategy. `TestReloadUpdatesDomainStrategy` and
`TestAppendingRulesKeepsTheStrategy`.

## Verifying the result

```sh
go list -deps -f '{{if .Module}}{{.Module.Path}}{{end}}' ./... | grep sagernet   # must be empty
grep sagernet go.mod go.sum                                                      # must be empty
```

Both are empty as of this commit. The remaining dependency set is MIT / BSD / Apache-2.0 /
MPL-2.0, with one exception worth naming: `github.com/juju/ratelimit` (pulled in by
`github.com/xtls/reality`) is **LGPL-3.0 with a static-linking exception** that explicitly
permits conveying a statically linked Combined Work without providing minimal corresponding
source. Attribution is still required.
