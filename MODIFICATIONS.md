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

## Letting the test suite run without the geodata downloads

Not a licence change, and not a behaviour change either — this one is only about the tests
being runnable.

| File | Change |
|---|---|
| `app/router/condition_geoip_test.go` | `getAssetPath` now wraps a sentinel `errNoAsset` when the file is simply absent. New helper `mustHaveAsset(tb, err)` skips on that sentinel and `common.Must`s anything else. Applied to `TestGeoIPMatcher4CN`, `TestGeoIPMatcher6US` and the two benchmarks beside them. |
| `app/router/condition_test.go` | Same helper applied to `TestChinaSites`, `BenchmarkMphDomainMatcher` and `BenchmarkMultiGeoIPMatcher`. |
| `infra/conf/router_test.go` | Its own copy of `getAssetPath` gets the same sentinel; `TestToCidrList` skips on it. |

`geoip.dat` and `geosite.dat` are about 10 MB together, are built by a separate release
workflow, and are in neither upstream's repository nor this fork's. Upstream's CI downloads them
before running tests, so upstream never meets the failure. A plain `go test ./...` on a fresh
clone does: `loadGeoIP` returned an error, the callers passed it to `common.Must`, and the
resulting **panic took down the entire `app/router` test binary** — so the price of not having a
10 MB download was that every other test in the package, including the three added above for the
copy-on-write change, reported nothing at all.

The distinction the sentinel draws is the whole point. **Absent** is a skip: the machine does not
have an optional download. **Present but unreadable, or corrupt, or missing the country asked
for** still fails loudly, exactly as before — a dat file that parses wrong is a real defect and
must not be quiet. Both states are exercised: with `XRAY_LOCATION_ASSET` pointed at a directory
holding the two files, all four tests run and pass; without it, they skip and the rest of the
package runs.

## Not handing the consumer's descriptor back to the kernel

Not a licence change. One line, and it broke a contract the consumer's whole ownership model
rests on.

| File | Change |
|---|---|
| `proxy/tun/tun_android.go` | `NewTun` no longer calls `unix.Close(fd)` when `SetNonblock` fails; it returns the error and leaves the descriptor alone. It also stops ignoring `strconv.Atoi`'s error, which previously fell back to fd 0. |

The descriptor belongs to the caller. `libzapcore`'s package comment states the contract the
Kotlin side is built on — "only marks the fd non-blocking, **it does not dup it**, and
`AndroidTun.Close` is empty" — and `VpnService.Builder.establish()` is what produced it, so its
lifetime is the Android side's to manage. Closing it here handed its *number* back to the
process while the owner still believed it held it: the next socket anything opened could take
that number, and gVisor would then read and write somebody's HTTPS connection as if it were the
tunnel. A native tombstone with no Java frame in it.

`SetNonblock` does not fail on an ordinary device. It fails under a stricter SELinux policy on a
custom ROM, which is exactly the population that cannot report the crash usefully.

The `Atoi` half is smaller and in the same line of reasoning: `platform.NewEnvFlag(...)` falls
back to `"0"`, and `SetNonblock(0, true)` *succeeds* — on stdin. Returning the error means the
tunnel fails to start instead of attaching the stack to the wrong file.

## Failing the dial when the socket could not be protected

Not a licence change. Behaviour, and deliberately fail-closed.

| File | Change |
|---|---|
| `transport/internet/system_dialer.go` | Both controller loops (the UDP `ListenConfig.Control` and the TCP `Dialer.Control`) now `return err` when a registered controller fails, instead of logging it and dialling anyway. |

The only controller the consumer registers is the one that hands the socket to Android's
`VpnService.protect()`. Upstream's behaviour — `errors.LogInfoInner` and carry on — produces a
working connection that was never protected, and says so nowhere any application can see.

What that costs depends on the consumer, and for this one it is *not* a routing loop: the app is
excluded from its own tunnel at UID level, so an unprotected socket dials out directly, which is
what protect was arranging anyway. What protect actually is here is depth — the line that still
holds when an OEM's UID rules do not, or when somebody later adds the app to its own allow-list.
A line that fails open is not a line. A failed dial, by contrast, is a state the tunnel already
knows how to report.

## Surviving teardown while connections are still in flight (FakeDNS)

Not a licence change. A cherry-pick of upstream `7ab0a3ccb7` ("FakeDNS: Little fix", 2026-05-01),
which landed five weeks after the version this fork pins.

| File | Change |
|---|---|
| `app/dns/fakedns/fake.go` | `Holder.Close` no longer nils `domainToIP`, `ipRange` and `mu`; `mu` becomes a value mutex so it cannot be nil; the rest of the upstream commit is taken verbatim. `fake_close_test.go` (ours, not upstream's) pins the property. |

`Holder.Close` used to tear the fields out from under every reader. The readers are hot paths —
`IsIPInIPPool` answers the sniffer's `fakedns` destination override on every connection,
`GetDomainFromFakeDNS` maps a sniffed fake address back to its name, `GetFakeIPForDomain` answers
every A query — and none of them checked. Instance teardown does not drain connections, so a
stop with traffic in flight was one nil dereference away from ending the process: a panic on a
core goroutine is not recoverable by anyone (the consumer's `guard` only covers its own entry
points), so the runtime aborts.

Measured, not theoretical: this is the crash behind three tombstones on the consumer's side —
`SIGSEGV addr=0x0` inside `net.(*IPNet).Contains`, re-raised as SIGABRT with the pc parked on
`runtime.raise`'s post-`tgkill` instruction. It fired deterministically the moment geo rules
were enabled, because `IpIfNonMatch` makes the router resolve every unmatched connection: the
probe slows, the connect path retries, and the retry's stop lands while the tunnel is at its
busiest. Nil-ing the fields freed nothing anyway — the Holder is unreachable once the instance
is gone, and the GC takes it whole.

## Starting the tun inbound after the features (FakeDNS, the other end)

Not a licence change. A cherry-pick of upstream `06b4931743` (PR #6275, "TUN inbound: Start TUN
by AlwaysOnInboundHandler", 2026-06-09), which landed ten weeks after the version this fork pins
and ships from upstream v26.6.22.

| File | Change |
|---|---|
| `proxy/tun/handler.go` | `Init` keeps only the context, tag, sniffing request, policy manager and dispatcher. Creating the tun, building the stack and starting both move to a new `Start`, and `Handler` now declares `common.Runnable`. The stack options read `t.policyManager` rather than the `pm` parameter that no longer reaches them. |
| `app/proxyman/inbound/always.go` | `Start` starts the proxy itself when it implements `common.Runnable`, before the workers. |
| `proxy/tun/handler_start_test.go` | Ours, not upstream's. Pins that `Init` leaves `tun` and `stack` nil. |

`Close` is *not* taken from upstream: this fork already made it nil-safe and ordered (see
"Fixing the tun inbound's teardown"), which is what upstream's `common.CloseIfExists` achieves
there.

The same crash signature as the section above — `SIGSEGV addr=0x0` inside
`net.(*IPNet).Contains`, reached through `fakedns.(*Holder).GetDomainFromFakeDNS` — but from the
opposite end of the holder's life. `Close` no longer nils the fields; **`Start` had not yet
filled them**. `NewFakeDNSHolderConfigOnly` builds a holder whose `ipRange` is nil and leaves it
that way until the feature's own `Start` runs, while `Init` brought the interface up and began
dispatching. The first packet through the tun was sniffed against a holder that had no pool yet.

Measured on the consumer's Android e2e suite, 2026-08-20: the core logged `starting with {...}`
and panicked **73 ms later**, on the gVisor stack's own goroutine
(`proxy/tun.(*stackGVisor).Start.func1.1`), taking the VPN service down with it. One occurrence
in eight tunnel starts — a packet has to arrive inside the window, so it is a race rather than a
certainty, which is why the scenario that caught it had passed on the four runs before.

Upstream declines to guard the holder: issue #6442 proposed exactly that and was closed with
"这是TUN入站启动过早的问题 已经修复TUN入站". Issue #6274 is the same panic reported from
Windows. Taking the ordering fix rather than adding a nil check keeps this fork on upstream's
line, so the eventual rebase drops the patch instead of fighting it.

## Fixing the tun UDP FullCone NAT panic and a domain-typed reply

Not a licence change. Cherry-picks of two upstream fixes to `proxy/tun/udp_fullcone.go`, taken
because upstream hit both of them as real production crashes and this fork's pin predates both.

**`send on closed channel` (upstream #5888, merged 2026-04-05, nine days after the `26.3.27`
commit this fork pins).** The original `HandlePacket` released `udpConnectionHandler`'s mutex
before sending on `conn.egress`; `connectionFinished` closes that same channel under the same
mutex. A packet for a flow and that flow's own teardown landing at nearly the same instant could
race the send against the close. Upstream hit this twice in production — XTLS/Xray-core#5895
("TUN Panic", several weeks of normal operation before it fired) and its duplicate #5930 ("经过
一晚后代理退出") — both with the identical trace, `panic: send on closed channel` at
`udp_fullcone.go:47`. The fix restructures the lock around an `RWMutex`: an existing flow is
found and written to under a *read* lock held across the send, so `connectionFinished`'s
exclusive lock (and therefore the `close`) cannot proceed until every in-flight send has
finished. Also bundled: the buffer channel grows from 16 to 1024 entries, a dropped packet logs
at debug instead of vanishing silently, and `packet` now carries its destination alongside the
payload rather than losing it, which is what let `ReadMultiBuffer`/`WriteMultiBuffer` be added
in the same change.

**Domain-typed reply address (part of upstream #6285, merged 2026-06-09; only the
`proxy/tun/udp_fullcone.go` hunk applies — the rest of that PR touches `proxy/wireguard`, which
this fork does not carry, see "Removing `proxy/wireguard`").** `WriteMultiBuffer` used
`b.UDP`'s address unconditionally as the reply destination. If an outbound ever hands back a
`net.Destination` carrying a domain rather than an IP — which XTLS/Xray-core#6279 shows
happening with the `dns` and `hysteria2` outbounds specifically, both of which answer through
`proxy/tun` on this fork's own path (`dns-out` is exactly how a FakeDNS-only reply gets back to
the tun) — `writePacket` has no address to build an IP/UDP header from. The fix keeps the
original target and logs instead of using the domain, rather than letting a downstream outbound
choose an address `writePacket` cannot use.

| File | Change |
|---|---|
| `proxy/tun/udp_fullcone.go` | Both fixes verbatim from upstream (`HandlePacket`'s `RWMutex` restructuring, `packet` gaining a `dest` alongside `data`, `ReadMultiBuffer`/`Read`/`WriteMultiBuffer`/deadline methods added, and the `IsDomain()` guard in `WriteMultiBuffer`). |
| `proxy/tun/stack_gvisor.go` | The UDP transport handler callback matches upstream's post-#5888 shape (`pkt.Clone()`, an invalid-address guard that logs and drops rather than the caller's own bounds check, always `return true`), **plus a `DecRef` upstream does not have** — see below. One line dropped rather than carried over: upstream's own PR left the old `if len(data) == 0 { return false }` in as a commented-out remnant; there is nothing for it to do once the invalid-address guard replaces its job, so it is gone rather than sitting here dead. |
| `proxy/tun/udp_fullcone_test.go` | Ours, not upstream's — see the next section, which reuses this file. |

**The `DecRef` is ours, and #5888 needs it.** Upstream writes the whole thing as one expression,
`pkt.Clone().Data().AsRange().ToSlice()`, and never releases what `Clone` handed it.
`PacketBuffer.Clone` is not a refcount bump on the same object: it takes a fresh `PacketBuffer`
out of `pkPool`, calls `buf.Clone()` — which for every view does `chunk.IncRef()` and takes a
`View` out of `viewPool` — and `InitRefs()` the result to 1. `Range.ToSlice` is already a
`make`-and-copy the caller owns outright, so by the end of that expression the clone is garbage
that nothing will ever `DecRef`: one `PacketBuffer`, one `View` per view and one held chunk
reference per UDP packet, none of which return to their pools. Go's GC reclaims the memory, so
this is allocation churn rather than an unbounded leak — but it is churn proportional to UDP
packet rate, on a tun where every DNS query and QUIC handshake is a UDP packet.

Kept as `Clone` + `DecRef` rather than dropped entirely, though **the `Clone` earns nothing**:
`ToSlice` copies synchronously, before `HandlePacket` is called, so the pre-#5888
`pkt.Data().AsRange().ToSlice()` is exactly equivalent and does none of this work. Keeping
upstream's shape and adding the one missing line makes the eventual rebase a no-op instead of a
conflict; deleting the `Clone` would be the faster code and can be done later, on purpose,
rather than as a side effect of taking a crash fix.

Not otherwise brought current with upstream `main`: the tun package there has since grown ICMPv4/
ICMPv6 inbound support (`handleICMPv4Packet`/`handleICMPv6Packet`, a `Tun` interface change,
`createStack` registering `icmp.NewProtocol4/6`) and, oddly, replaced the invalid-address guard
this cherry-pick keeps with a bare `panic(id)`. Both are unrelated to what these two fixes are
for and pulling either in is a separate decision, not a side effect of taking a crash fix.

## Aging the tun UDP NAT table independently of downstream handlers

Not a licence change. Behaviour fix, reasoned from the code rather than measured against a
live repro — see the caveat at the end. Builds on the cherry-picks immediately above: it needs
their `RWMutex` to stay race-free (see below), and the two are meant to be read together.

`proxy/tun/udp_fullcone.go`'s `udpConns` map has no timeout of its own, cherry-picks above
included — `SetDeadline`/`SetReadDeadline`/`SetWriteDeadline` are still no-ops upstream. A flow
is removed only when something calls `udpConn.Close()`, and the only caller of that is
`Handler.HandleConnection`'s `defer conn.Close()` — which does not run until
`dispatcher.DispatchLink` returns, i.e. until whatever downstream handler is proxying that flow
decides on its own that it is idle. `proxy/dns.Handler.Process` does this correctly, via
`ConnectionIdle` (300s by default) — but a flow that gets exactly one packet, the common case
for a DNS query answered out of a fake-IP pool, leaves both the map entry and
`handleConnection`'s goroutine (blocked reading `conn.egress`, which nothing will write to
again) alive for **twice** that, around 600s, before the downstream idle timer unwinds back to
the `defer`.

Twice, not once, and the doubling is not a rounding error: `signal.CancelAfterInactivity` is a
`task.Periodic` ticking at `ConnectionIdle`, whose `check()` only asks whether an `Update`
landed since the previous tick. `proxy/dns` plants one at `dns.go:191` when it reads the single
query, so the tick at t=300s is spent eating that token and only the tick at t=600s calls
`terminate`. (The token `SetTimeout` itself plants at `timer.go:74` is *not* the one that
survives — `task.Periodic.Start` runs the first `check()` synchronously at t=0 and eats it
immediately, by design: `SetTimeout` holds `t.mu` across `Start()` and `check()`→`finish()`
re-takes it, so an empty channel there would self-deadlock.) Measured on this tree by driving
the real `proxy/dns.Handler.Process` with a one-shot fake-IP query: with `ConnectionIdle` at
300ms, `Process` returned after 601ms, 2.00×.

Under a busy tun in "global mode" — every DNS lookup and every QUIC handshake attempt goes
through this same table — steady state is therefore (new-flow rate) × ~600s worth of these, all
sharing one lock.

Checked against upstream `main` (past the cherry-picks above): the architecture has not
changed since — no aging was ever added, just the `RWMutex`/buffer/logging work described
above. `sing`'s NAT table (`sagernet/sing/common/udpnat`, and its successor `common/udpnat2`)
does the opposite: the table itself is a TTL cache (`cache.LruCache` / `freelru.Cache`) that
evicts and closes on its own schedule, independent of whatever the downstream handler does;
sing-box exposes this as `udp_timeout`/`udp_nat_max` on its tun inbound. This patch gives
Xray's table the same property without a cache dependency — a `time.Ticker` owned by
`udpConnectionHandler` itself.

| File | Change |
|---|---|
| `proxy/tun/udp_fullcone.go` | `udpConn` gets `lastActive atomic.Int64`, touched on every packet `HandlePacket` accepts, in both the read-lock fast path and the write-lock slow path. `udpConnectionHandler` gets `idleTimeout`, `reapLoop` (a ticker at `idleTimeout/4`) and `reapExpired`, which closes anything `reapLoop` finds stale — through the existing `Close()` → `connectionFinished()` path, not a new one. `newUdpConnectionHandler` takes `idleTimeout` and starts the loop when it is positive; `Close()` stops it, idempotently (`sync.Once`), **and drains what is left** — see below. Both the sweep and the drain go through one `evict` helper. `connectionFinished` takes the `*udpConn` as well as its key — also below. |
| `proxy/tun/stack_gvisor.go` | `stackGVisor` keeps the `*udpConnectionHandler` it builds in `Start`, passes it `t.idleTimeout`, and calls the handler's `Close` from `stackGVisor.Close` — *after* `endpoint.Attach(nil)`, see below. |
| `proxy/tun/udp_fullcone_test.go` | Ours. Pins that an idle flow gets reaped, that an active one does not, that `Close` drains live flows, that a late second close cannot evict a reused source port, and — under `-race` — that a reap racing an ordinary packet cannot panic (a regression guard for the exact shape of #5895/#5930, since this patch calls `Close()` far more often than upstream ever did). |

**Why this is safe under the cherry-picked locking, not merely compatible with it.** `evict`
does the whole sweep — decide, `delete`, `close(conn.egress)` — under one exclusive `Lock()`,
which is the same lock upstream's fix already serialises `HandlePacket`'s in-flight sends
against, so nothing here reopens the window #5888 closed. Deleting during `range` is legal and
`close` never blocks, so holding the lock across the sweep costs nothing. It deliberately does
*not* route through `conn.Close()`: that re-enters this same lock via `connectionFinished`, and
a `sync.RWMutex` is not reentrant. The obvious alternative — collect under `RLock`, release,
then `conn.Close()` each one — avoids the deadlock but reintroduces a check-then-act window, in
which a packet can arrive on a flow already marked stale, be accepted onto its `egress`, and
have the flow torn down under it anyway.

**`Close` drains, and the ordering in `stackGVisor.Close` is what makes the drain complete.**
`endpoint.Attach(nil)` calls `Stop()` on every inbound dispatcher and then `Wait()`s for them to
leave `dispatchLoop`, so once it returns nothing can deliver another packet. Draining after it
is therefore final; draining before it would leave anything that arrived in the gap sitting in
the table with the reaper already stopped. Measured before the drain existed: 100 one-shot
flows, `Close()`, then ten times `idleTimeout` — still 100 entries and 100 blocked goroutines.
`TestCloseDrainsLiveFlows` pins it and fails without the drain.

Worth being precise about what this is: not a regression the reaper introduced, since a
pre-patch shutdown strands the same flows and more (nothing aged the table during the session
either). It is an improvement the first draft left unrealised — `Close()` stopped the one
mechanism that could have collected them and put nothing in its place.

**Calling it far more often is what forced the `connectionFinished` signature change.** Every
reaped flow is now closed *twice*: once by `reapExpired`, and again by `HandleConnection`'s own
`defer conn.Close()` once that first close unblocks the goroutine's read. Upstream only ever
closed once, so keying the eviction purely on `src` was fine there. Here the second close can
land after the same source port has been handed to a brand-new flow — an ephemeral port that
just sat idle long enough to be reaped is exactly a port the OS is free to reuse — and a
`src`-keyed eviction would then delete and `close(egress)` on a live conn that had just started,
killing a working flow for no reason a user could ever connect to a cause. `connectionFinished`
therefore takes the `*udpConn` too and evicts only when the map still holds *that* conn;
a late second close for a replaced key is a no-op. `TestSecondCloseDoesNotEvictAReusedSource`
pins it, and fails (panicking in `close(conn.egress)`) with the identity check removed.

**`StackOptions.IdleTimeout` was already there and already unused.** `stack_gvisor.go` has
stored `options.IdleTimeout` in a field named `idleTimeout` since that field existed, and
nothing ever read it — dead configuration that looks like it was meant for exactly this. It is
populated in `handler.go`'s `Start` from
`t.policyManager.ForLevel(t.config.UserLevel).Timeouts.ConnectionIdle` — the identical
expression `proxy/dns` reads its own timeout from at `dns.go:59` — so the reap interval tracks
whatever `ConnectionIdle` a caller's policy config sets, 300s by default.

That is the same *setting*, but deliberately not the same *dwell*. Because `reapLoop` tickets
at `idleTimeout/4` and compares strictly, a flow is reaped in (300s, 375s] after its last
packet, against the ~600s established above for `proxy/dns` — so this is not merely a
redundant second enforcer of a deadline something else already met, it roughly halves the
worst case. And for flows whose downstream handler has no idle deadline at all — a QUIC
handshake through the `freedom` outbound is the obvious one — it is the only bound there is.

Not measured against a live repro: reasoned from the code path, not pulled from a goroutine
dump taken during an actual long-running session. If it does not move the needle on the
consumer-side report this was written for (zapsplit-v2 issue #57 — DNS resolution failing
roughly 20 minutes into a "global mode" connection), the next thing to check is whether
`udpConns`'s size and Go's live goroutine count actually grow unboundedly before this patch and
plateau after it.

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
